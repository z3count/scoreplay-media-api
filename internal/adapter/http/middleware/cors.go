// Package middleware — cors.go implements Cross-Origin Resource Sharing policy.
//
// Security fix: Missing CORS policy.
// Without CORS headers, browsers block cross-origin requests from frontends.
// A wildcard (*) policy would expose the API to any website. This middleware
// restricts access to explicitly allowed origins.
package middleware

import (
	"net/http"
	"strings"
)

// CORS returns a middleware that enforces a Cross-Origin Resource Sharing policy.
//
// Restricts cross-origin access to only the specified allowed origins.
// If allowedOrigins is empty, CORS headers are not set (most restrictive).
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	originSet := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[strings.TrimRight(o, "/")] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" && originSet[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
				w.Header().Set("Access-Control-Max-Age", "300")
				w.Header().Set("Vary", "Origin")
			}

			// Handle preflight requests.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
