package main

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/maci0/katamaran/internal/controller"
)

// serveDebug exposes /healthz, /readyz, /metrics, and /debug/vars (expvar).
// Failure to listen is fatal because Kubernetes uses these probes for liveness.
func serveDebug(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	plainOK := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body + "\n"))
		}
	}
	mux.HandleFunc("GET /healthz", plainOK("ok"))
	mux.HandleFunc("GET /readyz", plainOK("ready"))
	mux.Handle("GET /debug/vars", expvar.Handler())
	mux.HandleFunc("GET /metrics", servePrometheusMetrics)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("Debug HTTP server shutdown error", "error", err)
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail(fmt.Errorf("debug server: %w", err))
	}
}

// servePrometheusMetrics writes controller expvar counters in Prometheus
// text-exposition format without adding a metrics dependency.
func servePrometheusMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	type metricSpec struct {
		help, kind string
	}
	specs := map[string]metricSpec{
		"katamaran_migrations_dispatched_total":          {"Migrations the controller has dispatched (Apply succeeded).", "counter"},
		"katamaran_migrations_succeeded_total":           {"Migrations that reached PhaseSucceeded.", "counter"},
		"katamaran_migrations_failed_total":              {"Migrations that reached PhaseFailed.", "counter"},
		"katamaran_migrations_recovered_total":           {"Migrations the controller resumed observing after a restart.", "counter"},
		"katamaran_migrations_resumed_total":             {"Migrations whose dest Job was (re-)created via Orchestrator.Resume during restart recovery.", "counter"},
		"katamaran_migrations_deleted_total":             {"Migration CRs the controller cleaned up via finalizer.", "counter"},
		"katamaran_migrations_inflight":                  {"Migrations currently in a non-terminal phase.", "gauge"},
		"katamaran_migrations_reconcile_errors_total":    {"Reconcile loop errors observed since startup.", "counter"},
		"katamaran_migrations_status_patch_errors_total": {"Migration status subresource patch failures observed since startup.", "counter"},
		"katamaran_migrations_watch_lost_total":          {"Watch channels that closed before reaching a terminal phase.", "counter"},
		"katamaran_migrations_worker_panics_total":       {"Recovered panics in dispatch/recover goroutines.", "counter"},
	}
	expvar.Do(func(kv expvar.KeyValue) {
		spec, ok := specs[kv.Key]
		if !ok {
			return
		}
		raw := kv.Value.String()
		fmt.Fprintf(w, "# HELP %s %s\n", kv.Key, spec.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", kv.Key, spec.kind)
		fmt.Fprintf(w, "%s %s\n", kv.Key, raw)
	})

	snap := controller.MigrationProgressSnapshot()
	if len(snap) == 0 {
		return
	}
	emitIntGauge := func(name, help string, get func(controller.MigrationProgressEntry) int64) {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
		fmt.Fprintf(w, "# TYPE %s gauge\n", name)
		for id, e := range snap {
			fmt.Fprintf(w, "%s{migration_id=%q} %d\n", name, id, get(e))
		}
	}
	emitIntGauge("katamaran_migration_ram_transferred_bytes", "RAM bytes transferred so far.",
		func(e controller.MigrationProgressEntry) int64 { return e.RAMTransferred })
	emitIntGauge("katamaran_migration_ram_total_bytes", "Total RAM bytes to transfer.",
		func(e controller.MigrationProgressEntry) int64 { return e.RAMTotal })
	fmt.Fprintf(w, "# HELP katamaran_migration_phase Current migration phase.\n")
	fmt.Fprintf(w, "# TYPE katamaran_migration_phase gauge\n")
	for id, e := range snap {
		fmt.Fprintf(w, "katamaran_migration_phase{migration_id=%q,phase=%q} 1\n", id, e.Phase)
	}
	emitIntGauge("katamaran_migration_downtime_ms", "Actual VM pause duration in milliseconds.",
		func(e controller.MigrationProgressEntry) int64 { return e.DowntimeMS })
	emitIntGauge("katamaran_migration_applied_downtime_ms", "Configured downtime limit in milliseconds.",
		func(e controller.MigrationProgressEntry) int64 { return e.AppliedDowntimeMS })
	emitIntGauge("katamaran_migration_rtt_ms", "Measured round-trip time in milliseconds.",
		func(e controller.MigrationProgressEntry) int64 { return e.RTTMS })
}

func validListenAddr(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	_, err = net.LookupPort("tcp", port)
	return err == nil
}
