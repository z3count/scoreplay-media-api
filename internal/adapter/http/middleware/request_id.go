package middleware

import (
	"net/http"

	"github.com/google/uuid"
)

// RequestID returns a middleware that ensures every request has a unique
// X-Request-ID header, either from the client or generated server-side.
//
// The request ID is used for:
//   - Correlating log entries across a single request's lifecycle.
//   - Tracing requests across microservices (if the ID is forwarded).
//   - Debugging: when a user reports an error, the request ID helps locate
//     the exact log entries.
//
// Behavior:
//   - If the incoming request already has an X-Request-ID header (e.g., set
//     by a reverse proxy or load balancer), it is preserved.
//   - If no X-Request-ID is present, a new UUID v4 is generated.
//   - The request ID is always set on the response headers so the client
//     can reference it in bug reports.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.New().String()
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}
