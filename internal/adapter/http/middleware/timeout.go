package middleware

import (
	"context"
	"net/http"
	"time"
)

// Timeout returns a middleware that sets a context deadline on each request.
//
// When the deadline expires, the request's context is cancelled, which
// propagates to database queries, storage operations, and any other
// context-aware code downstream. This prevents:
//   - Slow uploads holding DB connections and goroutines indefinitely.
//   - Runaway queries consuming database resources.
//   - Stuck HTTP clients keeping connections alive forever.
//
// The timeout does NOT forcibly kill the goroutine or close the connection —
// it relies on downstream code to respect context cancellation. All repository
// and storage methods in this project already accept context.Context.
//
// Note: this middleware uses context.WithTimeout rather than http.TimeoutHandler
// because http.TimeoutHandler buffers the entire response body in memory and
// does not support streaming (e.g., file downloads). Context-based timeout is
// lighter and works cooperatively with our handler code.
//
// The timeout value should be longer than the expected max request duration
// but short enough to prevent resource leaks. A good default is 30 seconds
// for API endpoints that include file uploads.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
