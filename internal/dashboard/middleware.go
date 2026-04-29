package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// contextKey is an unexported type for context keys in this package,
// preventing collisions with keys defined in other packages.
type contextKey string

const requestIDKey contextKey = "request_id"

// requestIDFromContext returns the request ID stored in the context,
// or an empty string if none is set.
func requestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// generateID returns a random 16-character hex string suitable for
// request and migration correlation IDs.
func generateID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read failing indicates a broken system (no entropy).
		// Panic rather than silently producing zero-value IDs that break correlation.
		panic("crypto/rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// responseWriter wraps http.ResponseWriter to capture the status code and
// response body size for request logging.
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytesOut    int
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.status = code
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesOut += n
	return n, err
}

// maxRequestIDLen is the maximum length of a client-supplied X-Request-Id header.
const maxRequestIDLen = 128

// slowRequestThreshold is the duration above which HTTP requests are logged
// at WARN level instead of INFO, making them visible to alerting systems.
const slowRequestThreshold = 5 * time.Second

// validRequestID checks that a client-supplied request ID is safe to propagate.
// Rejects IDs that are too long or contain non-printable/non-ASCII characters
// to prevent log pollution and header abuse.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// requestLogger wraps an http.Handler to log each request at completion.
// It propagates the X-Request-Id header from the client or generates one,
// enabling correlation across log entries for the same request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		reqID := r.Header.Get("X-Request-Id")
		if !validRequestID(reqID) {
			reqID = generateID()
		}

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		rw.Header().Set("X-Request-Id", reqID)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, reqID))
		next.ServeHTTP(rw, r)
		duration := time.Since(start)
		recordHTTPRequest(r.URL.Path, rw.status, duration)
		// Skip logging health, readiness, metrics, and debug scrape paths.
		if isObservabilityPath(r.URL.Path) {
			return
		}
		slow := duration >= slowRequestThreshold
		failed := rw.status >= http.StatusInternalServerError
		// Suppress INFO logs for the dashboard's 1-Hz status poll. Errors and
		// slow responses still surface via the WARN path; metrics still record.
		if r.URL.Path == "/api/status" && !slow && !failed {
			return
		}
		attrs := make([]any, 0, 16)
		attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
		if r.URL.RawQuery != "" {
			attrs = append(attrs, "has_query", true)
		}
		attrs = append(attrs, "status", rw.status, "elapsed", duration.Round(time.Millisecond), "bytes_out", rw.bytesOut, "content_length", r.ContentLength, "remote_addr", r.RemoteAddr, "request_id", reqID)
		if slow || failed {
			slog.Warn("Slow or failed HTTP request", attrs...)
		} else {
			slog.Info("HTTP request", attrs...)
		}
	})
}

// recoverMiddleware catches panics in HTTP handlers, logs them, and returns 500.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("HTTP handler panic", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()), "request_id", requestIDFromContext(r.Context()))
				if strings.HasPrefix(r.URL.Path, "/api/") {
					jsonError(w, "Internal server error", http.StatusInternalServerError)
				} else {
					http.Error(w, "Internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// csrfCheck rejects cross-origin state-changing requests. It validates the
// Origin header first; if absent, it falls back to the Referer header.
// Requests with neither header (e.g., curl, scripts) are allowed through,
// since non-browser clients cannot be CSRF'd. Modern browsers also send
// Sec-Fetch-Site; if it indicates cross-site/cross-origin, the request is
// rejected even if Origin/Referer somehow pass.
func csrfCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			// Sec-Fetch-Site is browser-set and unforgeable from script. The
			// only values that prove the request originated from our exact
			// origin (or a non-browser caller) are "same-origin" and "none";
			// "same-site" still allows a sibling subdomain to attack us.
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
				slog.Warn("CSRF check rejected request (sec-fetch-site)", "method", r.Method, "path", r.URL.Path, "sec_fetch_site", sfs, "remote_addr", r.RemoteAddr, "request_id", requestIDFromContext(r.Context()))
				csrfForbidden(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					slog.Warn("CSRF check rejected request", "method", r.Method, "path", r.URL.Path, "origin", origin, "host", r.Host, "remote_addr", r.RemoteAddr, "request_id", requestIDFromContext(r.Context()))
					csrfForbidden(w, r)
					return
				}
			} else if ref := r.Header.Get("Referer"); ref != "" {
				// Fallback: some browser edge cases omit Origin but include
				// Referer. If Referer is present, validate its host matches.
				u, err := url.Parse(ref)
				if err != nil || u.Host != r.Host {
					refHost := ""
					if u != nil {
						refHost = u.Host
					}
					slog.Warn("CSRF check rejected request (referer)", "method", r.Method, "path", r.URL.Path, "referer_host", refHost, "host", r.Host, "remote_addr", r.RemoteAddr, "request_id", requestIDFromContext(r.Context()))
					csrfForbidden(w, r)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func csrfForbidden(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		jsonError(w, "Forbidden", http.StatusForbidden)
	} else {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}
}

// securityHeaders wraps an http.Handler to set standard security headers
// on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com/3.4.17 https://cdn.jsdelivr.net/npm/chart.js@4.5.1/dist/chart.umd.min.js; "+
				"style-src 'self' 'unsafe-inline' https://cdn.tailwindcss.com; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"base-uri 'self'; "+
				"form-action 'self'; "+
				"frame-ancestors 'none'; "+
				"object-src 'none'")
		next.ServeHTTP(w, r)
	})
}
