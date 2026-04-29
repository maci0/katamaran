package dashboard

import (
	"encoding/json"
	"expvar"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maci0/katamaran/internal/buildinfo"
)

var (
	dashboardHTTPRequestsTotal          = expvar.NewInt("dashboard_http_requests_total")
	dashboardHTTPServerErrorsTotal      = expvar.NewInt("dashboard_http_server_errors_total")
	dashboardHTTPSlowRequestsTotal      = expvar.NewInt("dashboard_http_slow_requests_total")
	dashboardHTTPRequestDurationMSTotal = expvar.NewInt("dashboard_http_request_duration_ms_total")
	dashboardHTTPResponsesByStatusClass = expvar.NewMap("dashboard_http_responses_by_status_class")
	dashboardHTTPRequestDurationBuckets = expvar.NewMap("dashboard_http_request_duration_ms_buckets")
	dashboardReadinessFailuresTotal     = expvar.NewInt("dashboard_readiness_failures_total")

	dashboardMigrationsActive           = expvar.NewInt("dashboard_migrations_active")
	dashboardMigrationDurationMSTotal   = expvar.NewInt("dashboard_migration_duration_ms_total")
	dashboardMigrationDurationMSBuckets = expvar.NewMap("dashboard_migration_duration_ms_buckets")
	dashboardMigrationResultsByOutcome  = expvar.NewMap("dashboard_migration_results_by_outcome")
)

func recordHTTPRequest(path string, status int, duration time.Duration) {
	if isObservabilityPath(path) {
		return
	}
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

func durationBucket(duration time.Duration) string {
	switch {
	case duration < 100*time.Millisecond:
		return "lt_100ms"
	case duration < 500*time.Millisecond:
		return "lt_500ms"
	case duration < time.Second:
		return "lt_1s"
	case duration < slowRequestThreshold:
		return "lt_5s"
	default:
		return "gte_5s"
	}
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

	writePromMetric(w, "dashboard_http_requests_total", "Dashboard HTTP requests served, excluding health and metrics endpoints.", "counter", dashboardHTTPRequestsTotal.String())
	writePromMetric(w, "dashboard_http_server_errors_total", "Dashboard HTTP responses with status code >= 500.", "counter", dashboardHTTPServerErrorsTotal.String())
	writePromMetric(w, "dashboard_http_slow_requests_total", "Dashboard HTTP requests slower than the configured slow request threshold.", "counter", dashboardHTTPSlowRequestsTotal.String())
	writePromMetric(w, "dashboard_http_request_duration_ms_total", "Sum of observed dashboard HTTP request durations in milliseconds.", "counter", dashboardHTTPRequestDurationMSTotal.String())
	writePromMapMetric(w, "dashboard_http_responses_total", "Dashboard HTTP responses by status class.", "counter", "status_class", dashboardHTTPResponsesByStatusClass)
	writePromMapMetric(w, "dashboard_http_request_duration_ms_bucket_total", "Dashboard HTTP request duration bucket counts.", "counter", "bucket", dashboardHTTPRequestDurationBuckets)
	writePromMetric(w, "dashboard_readiness_failures_total", "Dashboard readiness checks that failed because the orchestrator was unavailable.", "counter", dashboardReadinessFailuresTotal.String())
	writePromMetric(w, "dashboard_migrations_active", "Dashboard migrations currently running.", "gauge", dashboardMigrationsActive.String())
	writePromMetric(w, "dashboard_migration_duration_ms_total", "Sum of completed dashboard migration durations in milliseconds.", "counter", dashboardMigrationDurationMSTotal.String())
	writePromMapMetric(w, "dashboard_migration_duration_ms_bucket_total", "Completed dashboard migration duration bucket counts.", "counter", "bucket", dashboardMigrationDurationMSBuckets)
	writePromMapMetric(w, "dashboard_migration_results_total", "Completed dashboard migrations by outcome.", "counter", "outcome", dashboardMigrationResultsByOutcome)
}

func writePromMetric(w http.ResponseWriter, name, help, kind, value string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
	fmt.Fprintf(w, "%s %s\n", name, value)
}

func writePromMapMetric(w http.ResponseWriter, name, help, kind, labelName string, m *expvar.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
	m.Do(func(kv expvar.KeyValue) {
		fmt.Fprintf(w, "%s{%s=%q} %s\n", name, labelName, kv.Key, kv.Value.String())
	})
}

// publishExpvars wires the dashboard's runtime counters into the
// process-wide expvar registry. Run() can be invoked more than once
// per process (the test suite does), so we use expvar.Get to detect
// already-registered names and reuse them — expvar.NewString /
// expvar.Publish panic on duplicate registration.
//
// The handler functions captured here are bound to the live App, so
// re-running Run() with a fresh App means subsequent /debug/vars
// scrapes report counters from the new instance. That matches the
// "tests reuse Run with their own App" semantics; a stale closure
// would be misleading otherwise.
func publishExpvars(app *App) {
	if v, ok := expvar.Get("version").(*expvar.String); ok {
		v.Set(buildinfo.Version)
	} else {
		expvar.NewString("version").Set(buildinfo.Version)
	}
	publishExpvarFunc("migrations_started", func() any { return app.getCounter("started") })
	publishExpvarFunc("migrations_succeeded", func() any { return app.getCounter("succeeded") })
	publishExpvarFunc("migrations_failed", func() any { return app.getCounter("failed") })
}

// publishExpvarFunc registers fn under name, or replaces the existing
// expvar.Func with the new closure when name is already registered.
// The underlying expvar.Map exposes no Set on the registered Var, so
// we shadow it via a stable wrapper variable per name and just swap
// the pointed-at function on subsequent Run() invocations.
func publishExpvarFunc(name string, fn func() any) {
	if w, ok := expvar.Get(name).(*expvarFuncWrapper); ok {
		w.set(fn)
		return
	}
	w := &expvarFuncWrapper{}
	w.set(fn)
	expvar.Publish(name, w)
}

// expvarFuncWrapper is an expvar.Var whose underlying function can be
// rebound. expvar.Func itself is a function value, not a struct, so
// once published its closure is fixed for the lifetime of the process.
// Wrapping it in a struct + atomic swap lets Run() be called again
// (e.g. from a test) without panicking on duplicate registration and
// without leaking the previous App's counter state into subsequent
// scrapes.
type expvarFuncWrapper struct {
	mu sync.Mutex
	fn func() any
}

func (w *expvarFuncWrapper) set(fn func() any) {
	w.mu.Lock()
	w.fn = fn
	w.mu.Unlock()
}

func (w *expvarFuncWrapper) String() string {
	w.mu.Lock()
	fn := w.fn
	w.mu.Unlock()
	if fn == nil {
		return "null"
	}
	v := fn()
	b, err := json.Marshal(v)
	if err != nil {
		return strconv.Quote(fmt.Sprintf("%v", v))
	}
	return string(b)
}

func (w *expvarFuncWrapper) Value() any {
	w.mu.Lock()
	fn := w.fn
	w.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}
