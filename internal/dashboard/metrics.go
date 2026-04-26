package dashboard

import (
	"expvar"
	"time"
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
	if path == "/healthz" || path == "/readyz" {
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
