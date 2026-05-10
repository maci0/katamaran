// Package dashboard implements the katamaran web UI HTTP server.
//
// It owns the transport layer, request validation, in-memory UI state,
// load-generation endpoints, and the wiring to the orchestrator package.
// Migration orchestration rules remain in internal/orchestrator so the
// dashboard, controller, and structured CLI share one application boundary.
package dashboard
