package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON sends a JSON response with the given status code. Sets
// Cache-Control: no-store on every JSON response — all dashboard JSON
// payloads are dynamic and must not be cached by intermediaries.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("Failed to encode JSON response", "error", err, "status", status)
	}
}

// jsonError sends a JSON error response: {"error": "..."}.
func jsonError(w http.ResponseWriter, msg string, status int) {
	writeJSON(w, status, map[string]string{"error": msg})
}
