package dashboard

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
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

// parseFormPOST validates the request Content-Type and parses the form body
// (capped at maxBodySize). On failure it writes the appropriate JSON error
// response and returns false. logCtx is included in WARN logs to identify
// the calling endpoint.
func parseFormPOST(w http.ResponseWriter, r *http.Request, logCtx string) bool {
	reqID := requestIDFromContext(r.Context())
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			slog.Warn(logCtx+": unsupported content type", "content_type", ct, "request_id", reqID)
			jsonError(w, "Content-Type must be application/x-www-form-urlencoded", http.StatusUnsupportedMediaType)
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := r.ParseForm(); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			slog.Warn(logCtx+": request body too large", "request_id", reqID)
			jsonError(w, "Request body too large", http.StatusRequestEntityTooLarge)
		} else {
			slog.Warn(logCtx+": failed to parse form body", "error", err, "request_id", reqID)
			jsonError(w, "Invalid request body", http.StatusBadRequest)
		}
		return false
	}
	return true
}
