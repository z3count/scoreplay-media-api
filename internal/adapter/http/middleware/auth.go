// Package middleware — auth.go implements API key authentication.
//
// Security fix: OWASP A01:2021 — Broken Access Control
// Without authentication, any anonymous user can create, read, and delete
// resources. This middleware enforces API key verification on all protected
// endpoints, rejecting unauthenticated requests with 401 Unauthorized.
//
// The API key is read from the X-API-Key header (preferred for programmatic
// clients) or the Authorization: Bearer <key> header (standard OAuth2-style).
//
// When no API key is configured (empty string), the middleware is a no-op,
// allowing unprotected local development. In production, the API_KEY
// environment variable MUST be set.
package middleware

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// APIKeyAuth returns a middleware that validates API key authentication.
//
// Security: uses constant-time comparison (subtle.ConstantTimeCompare) to
// prevent timing attacks that could leak the key length or prefix.
//
// If apiKey is empty, authentication is disabled (development mode).
//
// OWASP Logging: all authentication failures are logged with structured
// fields (remote_addr, method, path) to enable brute-force detection
// and security monitoring.
func APIKeyAuth(apiKey string, logger ...*slog.Logger) func(http.Handler) http.Handler {
	// Accept optional logger to maintain backward compatibility.
	var log *slog.Logger
	if len(logger) > 0 && logger[0] != nil {
		log = logger[0]
	}
	metrics := NewMetrics()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Security: Skip auth when no key is configured (dev mode only).
			if apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}

			provided := extractAPIKey(r)
			if provided == "" {
				// OWASP: Log missing API key attempts for brute-force detection.
				metrics.IncAuthFailure("missing")
				if log != nil {
					log.Warn("auth failure: missing API key",
						"remote_addr", r.RemoteAddr,
						"method", r.Method,
						"path", r.URL.Path,
					)
				}
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing API key")
				return
			}

			// Security: Constant-time comparison prevents timing side-channel attacks.
			if subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
				// OWASP: Log invalid API key attempts for brute-force detection.
				// Do NOT log the provided key to avoid leaking credentials.
				metrics.IncAuthFailure("invalid")
				if log != nil {
					log.Warn("auth failure: invalid API key",
						"remote_addr", r.RemoteAddr,
						"method", r.Method,
						"path", r.URL.Path,
					)
				}
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid API key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// extractAPIKey reads the API key from the request, checking both the
// X-API-Key header and the Authorization: Bearer <key> header.
func extractAPIKey(r *http.Request) string {
	// Preferred: X-API-Key header (simple, unambiguous).
	if key := r.Header.Get("X-API-Key"); key != "" {
		return key
	}

	// Fallback: Authorization: Bearer <key> (standard OAuth2 convention).
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}

	return ""
}

// writeAuthError writes a JSON error response for authentication failures.
// Separated from handler's writeError to avoid import cycles.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Minimal JSON encoding without importing encoding/json to keep this lean.
	w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + message + `"}}`)) //nolint:errcheck
}
