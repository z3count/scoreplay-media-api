// Package middleware provides HTTP middleware for cross-cutting concerns.
//
// Middleware functions wrap handlers to add behavior without modifying the
// handler logic itself. They follow the standard Go middleware pattern:
//   func(next http.Handler) http.Handler
//
// Middleware is applied in the router setup and executes in the order it is
// registered (outermost first).
package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger returns a middleware that logs every HTTP request using structured
// logging (slog).
//
// Logged fields:
//   - method: HTTP method (GET, POST, etc.)
//   - path: request path
//   - status: HTTP response status code
//   - duration: request processing time
//   - remote_addr: client IP address
//   - request_id: X-Request-ID header (if present, set by RequestID middleware)
//
// The middleware wraps the ResponseWriter to capture the status code, since
// Go's default ResponseWriter does not expose it after WriteHeader is called.
//
// Performance: the overhead of this middleware is negligible (one slog call
// per request). In ultra-high-throughput scenarios, sampling could be added.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(ww, r)

			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.status,
				"duration", time.Since(start).String(),
				"remote_addr", r.RemoteAddr,
				"request_id", r.Header.Get("X-Request-ID"),
			)
		})
	}
}

// statusWriter wraps http.ResponseWriter to capture the status code.
// This is necessary because the standard ResponseWriter does not expose the
// status code after WriteHeader has been called.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}
