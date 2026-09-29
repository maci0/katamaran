package dashboard

import (
	"bufio"
	"expvar"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/maci0/katamaran/internal/buildinfo"
)

// Metric names are declared once and used both to register the expvar and
// to write the Prometheus exposition. A literal repeated in both places can
// drift apart silently: expvar.Int still answers under the registered name
// while the scrape publishes the other one.
const (
	metricHTTPRequests          = "dashboard_http_requests_total"
	metricHTTPServerErrors      = "dashboard_http_server_errors_total"
	metricHTTPSlowRequests      = "dashboard_http_slow_requests_total"
	metricHTTPRequestDurationMS = "dashboard_http_request_duration_ms_total"
	metricReadinessFailures     = "dashboard_readiness_failures_total"
	metricCSRFRejections        = "dashboard_csrf_rejections_total"

	metricMigrationsActive      = "dashboard_migrations_active"
	metricMigrationApplyErrors  = "dashboard_migration_apply_errors_total"
	metricMigrationDurationMS   = "dashboard_migration_duration_ms_total"
	metricMigrationWatchErrors  = "dashboard_migration_watch_errors_total"
	metricMigrationWatchLost    = "dashboard_migration_watch_lost_total"
	metricMigrationWorkerPanics = "dashboard_migration_worker_panics_total"
)

var (
	dashboardHTTPRequestsTotal          = expvar.NewInt(metricHTTPRequests)
	dashboardHTTPServerErrorsTotal      = expvar.NewInt(metricHTTPServerErrors)
	dashboardHTTPSlowRequestsTotal      = expvar.NewInt(metricHTTPSlowRequests)
	dashboardHTTPRequestDurationMSTotal = expvar.NewInt(metricHTTPRequestDurationMS)
	dashboardHTTPResponsesByStatusClass = expvar.NewMap("dashboard_http_responses_by_status_class")
	dashboardHTTPRequestDurationBuckets = expvar.NewMap("dashboard_http_request_duration_ms_buckets")
	dashboardReadinessFailuresTotal     = expvar.NewInt(metricReadinessFailures)
	dashboardCSRFRejectionsTotal        = expvar.NewInt(metricCSRFRejections)

	dashboardMigrationsActive           = expvar.NewInt(metricMigrationsActive)
	dashboardMigrationApplyErrorsTotal  = expvar.NewInt(metricMigrationApplyErrors)
	dashboardMigrationDurationMSTotal   = expvar.NewInt(metricMigrationDurationMS)
	dashboardMigrationDurationMSBuckets = expvar.NewMap("dashboard_migration_duration_ms_buckets")
	dashboardMigrationResultsByOutcome  = expvar.NewMap("dashboard_migration_results_by_outcome")
	dashboardMigrationWatchErrorsTotal  = expvar.NewInt(metricMigrationWatchErrors)
	dashboardMigrationWatchLostTotal    = expvar.NewInt(metricMigrationWatchLost)
	dashboardMigrationWorkerPanicsTotal = expvar.NewInt(metricMigrationWorkerPanics)
)

func recordHTTPRequest(status int, duration time.Duration) {
	dashboardHTTPRequestsTotal.Add(1)
	dashboardHTTPResponsesByStatusClass.Add(statusClass(status), 1)
	dashboardHTTPRequestDurationMSTotal.Add(duration.Milliseconds())
	dashboardHTTPRequestDurationBuckets.Add(durationBucket(duration), 1)
	if status >= 500 {
		dashboardHTTPServerErrorsTotal.Add(1)
	}
	if duration >= slowRequestThreshold {
		dashboardHTTPSlowRequestsTotal.Add(1)
	}
}

func isObservabilityPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics", "/debug/vars":
		return true
	default:
		return strings.HasPrefix(path, "/debug/pprof/")
	}
}

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}

// durationBucket labels a request by how long it took. The last two labels
// name slowRequestThreshold itself rather than a hardcoded 5s, so a change to
// the threshold cannot leave the histogram labelling buckets the bound no
// longer uses.
func durationBucket(duration time.Duration) string {
	switch {
	case duration < 100*time.Millisecond:
		return "lt_100ms"
	case duration < 500*time.Millisecond:
		return "lt_500ms"
	case duration < time.Second:
		return "lt_1s"
	case duration < slowRequestThreshold:
		return "lt_" + bucketBoundLabel(slowRequestThreshold)
	default:
		return "gte_" + bucketBoundLabel(slowRequestThreshold)
	}
}

// bucketBoundLabel renders a bucket boundary as the Prometheus label
// fragment used in durationBucket ("5s", "1500ms").
func bucketBoundLabel(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// recordMigrationDuration records the wall-clock duration of a completed
// migration into the duration counter and histogram-style buckets, and
// increments the per-outcome result counter ("success" or "error").
func recordMigrationDuration(duration time.Duration, outcome string) {
	dashboardMigrationDurationMSTotal.Add(duration.Milliseconds())
	dashboardMigrationDurationMSBuckets.Add(migrationDurationBucket(duration), 1)
	if outcome != "" {
		dashboardMigrationResultsByOutcome.Add(outcome, 1)
	}
}

func migrationDurationBucket(d time.Duration) string {
	switch {
	case d < 30*time.Second:
		return "lt_30s"
	case d < 2*time.Minute:
		return "lt_2m"
	case d < 10*time.Minute:
		return "lt_10m"
	case d < 30*time.Minute:
		return "lt_30m"
	default:
		return "gte_30m"
	}
}

func serveDashboardMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// Buffer the many small Fprintf writes into one syscall per scrape.
	bw := bufio.NewWriter(w)
	defer func() {
		if err := bw.Flush(); err != nil {
			slog.Warn("metrics scrape flush failed", "error", err)
		}
	}()

	writePromMetric(bw, metricHTTPRequests, "Dashboard HTTP requests served, excluding health and metrics endpoints.", "counter", dashboardHTTPRequestsTotal.String())
	writePromMetric(bw, metricHTTPServerErrors, "Dashboard HTTP responses with status code >= 500.", "counter", dashboardHTTPServerErrorsTotal.String())
	writePromMetric(bw, metricHTTPSlowRequests, "Dashboard HTTP requests slower than the configured slow request threshold.", "counter", dashboardHTTPSlowRequestsTotal.String())
	writePromMetric(bw, metricHTTPRequestDurationMS, "Sum of observed dashboard HTTP request durations in milliseconds.", "counter", dashboardHTTPRequestDurationMSTotal.String())
	writePromMapMetric(bw, "dashboard_http_responses_total", "Dashboard HTTP responses by status class.", "status_class", dashboardHTTPResponsesByStatusClass)
	writePromMapMetric(bw, "dashboard_http_request_duration_ms_bucket_total", "Dashboard HTTP request duration bucket counts.", "bucket", dashboardHTTPRequestDurationBuckets)
	writePromMetric(bw, metricReadinessFailures, "Dashboard readiness checks that failed because the orchestrator was unavailable.", "counter", dashboardReadinessFailuresTotal.String())
	writePromMetric(bw, metricCSRFRejections, "Dashboard requests rejected by the CSRF middleware (cross-origin/cross-site state-changing requests).", "counter", dashboardCSRFRejectionsTotal.String())
	writePromMetric(bw, metricMigrationsActive, "Dashboard migrations currently running.", "gauge", dashboardMigrationsActive.String())
	writePromMetric(bw, metricMigrationApplyErrors, "Dashboard migration submissions rejected by the orchestrator Apply call.", "counter", dashboardMigrationApplyErrorsTotal.String())
	writePromMetric(bw, metricMigrationDurationMS, "Sum of completed dashboard migration durations in milliseconds.", "counter", dashboardMigrationDurationMSTotal.String())
	writePromMapMetric(bw, "dashboard_migration_duration_ms_bucket_total", "Completed dashboard migration duration bucket counts.", "bucket", dashboardMigrationDurationMSBuckets)
	writePromMapMetric(bw, "dashboard_migration_results_total", "Completed dashboard migrations by outcome.", "outcome", dashboardMigrationResultsByOutcome)
	writePromMetric(bw, metricMigrationWatchErrors, "Dashboard migrations where opening the orchestrator watch stream failed.", "counter", dashboardMigrationWatchErrorsTotal.String())
	writePromMetric(bw, metricMigrationWatchLost, "Dashboard migrations whose watch stream closed before a terminal status.", "counter", dashboardMigrationWatchLostTotal.String())
	writePromMetric(bw, metricMigrationWorkerPanics, "Recovered panics in the dashboard migration worker goroutine.", "counter", dashboardMigrationWorkerPanicsTotal.String())
}

func writePromMetric(w io.Writer, name, help, kind, value string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
	fmt.Fprintf(w, "%s %s\n", name, value)
}

func writePromMapMetric(w io.Writer, name, help, labelName string, m *expvar.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	m.Do(func(kv expvar.KeyValue) {
		fmt.Fprintf(w, "%s{%s=%q} %s\n", name, labelName, kv.Key, kv.Value.String())
	})
}

// expvarApp is the App the published counter gauges read from. The gauges
// dereference it on every scrape instead of closing over an App, so a later
// Run() with a fresh App rebinds them without republishing.
var expvarApp atomic.Pointer[App]

// publishExpvars wires the dashboard's runtime counters into the
// process-wide expvar registry. Run() can be invoked more than once per
// process (the test suite does) and expvar.Publish panics on duplicate
// registration, so the gauges are published once and every call rebinds
// expvarApp to the live App.
func publishExpvars(app *App) {
	expvarApp.Store(app)
	if v, ok := expvar.Get("version").(*expvar.String); ok {
		v.Set(buildinfo.Version)
	} else {
		expvar.NewString("version").Set(buildinfo.Version)
	}
	if expvar.Get("migrations_started") != nil {
		return
	}
	expvar.Publish("migrations_started", expvar.Func(func() any { s, _, _ := expvarApp.Load().counterSnapshot(); return s }))
	expvar.Publish("migrations_succeeded", expvar.Func(func() any { _, s, _ := expvarApp.Load().counterSnapshot(); return s }))
	expvar.Publish("migrations_failed", expvar.Func(func() any { _, _, f := expvarApp.Load().counterSnapshot(); return f }))
}
