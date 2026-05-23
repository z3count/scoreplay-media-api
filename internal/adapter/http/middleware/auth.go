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
// Multi-tenancy
// -------------
// The middleware delegates credential verification to a port.AuthVerifier,
// which returns a domain.Identity{TenantID, Scopes, …}. The identity is
// stashed in the request context via port.WithIdentity so handlers and
// repositories can scope every operation per-tenant. See DESIGN.md §3.
//
// Dev mode: if devTenantID is non-nil, an unauthenticated request is
// allowed and treated as if it came from that tenant with admin:* scope.
// This preserves the pre-tenancy behaviour where `API_KEY=""` disables
// auth for local development.
package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// APIKeyAuth returns a middleware that authenticates each request via the
// provided AuthVerifier and stashes the resulting Identity in the request
// context.
//
// Behaviour:
//   - No credential AND devTenantID != nil → pass with the dev identity.
//   - No credential AND devTenantID == nil → 401 (auth required).
//   - Credential present → call verifier.Verify; on success, stash identity.
//   - Verify returns ErrKeyExpired/ErrKeyRevoked → 401 with a specific reason.
//   - Verify returns any other error → 401 with reason "invalid".
//
// Failures are logged with slog (remote_addr only — never the key) and
// the http_auth_failures_total counter is bumped per reason label.
func APIKeyAuth(verifier port.AuthVerifier, devTenantID *uuid.UUID, logger *slog.Logger) func(http.Handler) http.Handler {
	metrics := NewMetrics()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := extractAPIKey(r)

			if provided == "" {
				if devTenantID != nil {
					// Dev mode: no credential required. Synthesize an
					// identity scoped to the dev tenant with full
					// privileges, so handlers proceed as before.
					next.ServeHTTP(w, r.WithContext(port.WithIdentity(r.Context(), domain.Identity{
						TenantID: *devTenantID,
						Scopes:   []string{"admin:*"},
					})))
					return
				}
				metrics.IncAuthFailure("missing")
				if logger != nil {
					logger.Warn("auth failure: missing API key",
						"remote_addr", r.RemoteAddr,
						"method", r.Method,
						"path", r.URL.Path,
					)
				}
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing API key")
				return
			}

			identity, err := verifier.Verify(r.Context(), provided)
			if err != nil {
				reason, message := classifyAuthError(err)
				metrics.IncAuthFailure(reason)
				if logger != nil {
					logger.Warn("auth failure",
						"reason", reason,
						"remote_addr", r.RemoteAddr,
						"method", r.Method,
						"path", r.URL.Path,
					)
				}
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHORIZED", message)
				return
			}

			next.ServeHTTP(w, r.WithContext(port.WithIdentity(r.Context(), identity)))
		})
	}
}

// classifyAuthError maps a verifier error to a low-cardinality reason
// label (used as the http_auth_failures_total{reason} value) and a
// safe client-facing message.
func classifyAuthError(err error) (reason, message string) {
	switch {
	case errors.Is(err, port.ErrKeyMissing):
		return "missing", "missing API key"
	case errors.Is(err, port.ErrKeyExpired):
		return "expired", "credential expired"
	case errors.Is(err, port.ErrKeyRevoked):
		return "revoked", "credential revoked"
	default:
		// ErrKeyInvalid + any internal error are folded into "invalid"
		// so an attacker can't probe the difference between "unknown
		// key" and "DB error" via the response.
		return "invalid", "invalid API key"
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
