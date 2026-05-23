package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// LegacyTenantID is the fixed UUID used to backfill all pre-existing rows
// during the tenancy migration. New deployments start with this row
// already in the database; legacy ones inherit it automatically. See
// migration 005_tenancy.up.sql.
var LegacyTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// TenantStatus is the lifecycle state of a tenant.
type TenantStatus string

const (
	TenantStatusActive    TenantStatus = "active"
	TenantStatusSuspended TenantStatus = "suspended"
)

// Tenant represents a billable / billable-eligible org calling the API
// (a club, league, federation, …). All domain rows belong to exactly one.
type Tenant struct {
	ID        uuid.UUID
	Name      string
	Status    TenantStatus
	CreatedAt time.Time
}

// APIKey is the persisted descriptor of a credential a tenant uses to
// authenticate. The raw key value is shown to the operator exactly once
// at creation time and never stored — only its SHA-256 hash lives here.
type APIKey struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	KeyHash    string // sha256 hex
	Name       string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

// Identity is the authenticated principal carried in request context after
// APIKeyAuth runs. Handlers and repositories read TenantID from it to
// scope every operation, and call HasScope to gate writes.
type Identity struct {
	TenantID  uuid.UUID
	KeyID     uuid.UUID
	Scopes    []string
	ExpiresAt *time.Time
}

// HasScope reports whether the identity has been granted scope. A
// "category:*" wildcard matches anything in that category, and the
// special "admin:*" matches everything. The check is exact-string for
// non-wildcard scopes — keep scope names short and stable.
func (i Identity) HasScope(scope string) bool {
	if scope == "" {
		return true
	}
	category := scope
	if idx := strings.IndexByte(scope, ':'); idx >= 0 {
		category = scope[:idx]
	}
	for _, granted := range i.Scopes {
		if granted == scope {
			return true
		}
		// "admin:*" is the catch-all super-scope.
		if granted == "admin:*" {
			return true
		}
		// "media:*" matches "media:write", "media:read", etc.
		if granted == category+":*" {
			return true
		}
	}
	return false
}
