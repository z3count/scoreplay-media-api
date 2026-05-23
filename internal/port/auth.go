// Package port — auth.go defines the authentication seam.
//
// The HTTP middleware extracts a credential from the request and calls
// AuthVerifier.Verify to turn it into an Identity. The middleware then
// stashes the Identity in the request context using IdentityFromContext /
// WithIdentity. Handlers and repositories never see raw credentials.
package port

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
)

// AuthVerifier turns a raw credential (today: an API key value) into an
// Identity. Implementations are responsible for hashing, DB lookup,
// expiry checks, and any caching. The credential is passed in plain so
// constant-time comparison can happen at the lowest level; callers must
// not log it.
type AuthVerifier interface {
	Verify(ctx context.Context, rawKey string) (domain.Identity, error)
}

// Sentinel errors returned by Verify. Middleware maps them to specific
// metric labels and 401/403 responses; do not wrap with extra context
// before returning, as that prevents errors.Is from matching.
var (
	ErrKeyMissing = errors.New("auth: missing credential")
	ErrKeyInvalid = errors.New("auth: invalid credential")
	ErrKeyExpired = errors.New("auth: credential expired")
	ErrKeyRevoked = errors.New("auth: credential revoked")
)

// identityCtxKey is the unexported context-key type used for Identity.
// Using a private struct{} prevents accidental collisions with other
// packages' context values.
type identityCtxKey struct{}

// WithIdentity returns a derived context carrying the Identity.
// Used by the auth middleware after a successful Verify.
func WithIdentity(ctx context.Context, id domain.Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the Identity stashed by WithIdentity, or
// the zero value with ok=false if none is present. Repositories that
// require a tenant scope should treat the !ok case as a programming
// error (the middleware must have run).
func IdentityFromContext(ctx context.Context) (domain.Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(domain.Identity)
	return id, ok
}

// ErrNoTenantContext indicates a repository or storage adapter was
// called without an Identity in context — i.e. the auth middleware did
// not run for this code path. It is a programming error, not a user
// error. Repositories return it rather than running an unscoped query.
var ErrNoTenantContext = errors.New("port: no tenant identity in context (auth middleware did not run?)")

// TenantIDFromContext extracts the TenantID from the request-scoped
// Identity. Returns ErrNoTenantContext if no Identity is present, which
// indicates a wiring bug rather than a missing credential.
func TenantIDFromContext(ctx context.Context) (uuid.UUID, error) {
	id, ok := IdentityFromContext(ctx)
	if !ok {
		return uuid.Nil, ErrNoTenantContext
	}
	return id.TenantID, nil
}
