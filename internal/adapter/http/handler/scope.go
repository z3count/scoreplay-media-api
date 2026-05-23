package handler

import (
	"net/http"

	"github.com/scoreplay/media-api/internal/port"
)

// Scopes recognised by this service. Naming convention:
// "<resource>:<verb>" — keep them stable; they appear in metric labels
// and in operator-issued API keys.
//
// admin:* in an Identity grants all of these (and any future ones).
const (
	ScopeMediaRead  = "media:read"
	ScopeMediaWrite = "media:write"
	ScopeTagsRead   = "tags:read"
	ScopeTagsWrite  = "tags:write"
)

// requireScope is the gate handlers use to enforce authorisation. It
// reads the Identity from context (placed there by APIKeyAuth), checks
// the scope, writes a 403 response and returns false on failure, or
// returns true on success.
//
// Callers use it as:
//
//	if !requireScope(w, r, ScopeMediaWrite) { return }
//
// If no Identity is in context the middleware didn't run — that's a
// wiring bug, not a credentials issue. Reject with 500 so it shows up
// loudly instead of silently authorising.
func requireScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	identity, ok := port.IdentityFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "auth middleware did not run")
		return false
	}
	if !identity.HasScope(scope) {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "missing scope: "+scope)
		return false
	}
	return true
}
