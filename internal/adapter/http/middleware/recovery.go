package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recovery returns a middleware that catches panics in downstream handlers
// and converts them to HTTP 500 responses instead of crashing the server.
//
// Why this matters:
//   - Without recovery, a panic in any handler kills the entire server process,
//     affecting all connected clients.
//   - With recovery, only the panicking request gets a 500; all other requests
//     continue to be served normally.
//   - The stack trace is logged for debugging but never sent to the client
//     (information disclosure prevention).
//
// This is especially important in production where:
//   - A nil pointer dereference in a rarely-hit code path should not bring
//     down the entire service.
//   - The stack trace helps developers find and fix the issue.
//
// Corner case: if the panic occurs after headers have been written (e.g.,
// during response streaming), the 500 status cannot be set. The client will
// receive a truncated response. This is unavoidable without buffering the
// entire response.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	metrics := NewMetrics()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					metrics.IncPanicRecovered()
					logger.Error("panic recovered",
						"panic", rec,
						"stack", string(debug.Stack()),
						"method", r.Method,
						"path", r.URL.Path,
						"request_id", r.Header.Get("X-Request-ID"),
					)
					http.Error(w, `{"error":{"code":"INTERNAL_ERROR","message":"an unexpected error occurred"}}`,
						http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
