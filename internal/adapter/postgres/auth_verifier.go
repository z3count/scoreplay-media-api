package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// AuthVerifier resolves a raw API key against the api_keys / tenants
// tables. The raw key is hashed with SHA-256 (hex) and compared against
// api_keys.key_hash. A short in-memory cache absorbs the common case of
// the same key being used many times per second.
//
// Why SHA-256 and not bcrypt: API keys are 256-bit random values issued
// by the system, not user-chosen passwords. The threat model is
// "attacker steals the DB and tries to reverse the hashes" — for
// high-entropy random values, plain SHA-256 is sufficient (brute-force
// is computationally infeasible against 256 random bits). bcrypt's
// per-comparison cost would also turn every authenticated request into
// a ~100 ms CPU stall.
type AuthVerifier struct {
	db    *sql.DB
	cache *keyCache
}

// NewAuthVerifier builds an AuthVerifier backed by db. cacheTTL controls
// how stale a cached Identity may be before a re-lookup. 5 s is a sane
// default — short enough that a revoked key stops working quickly,
// long enough to make the cache useful under load.
func NewAuthVerifier(db *sql.DB, cacheTTL time.Duration) *AuthVerifier {
	return &AuthVerifier{
		db:    db,
		cache: newKeyCache(cacheTTL),
	}
}

// Verify implements port.AuthVerifier. Returns one of:
//   - (Identity, nil)          on success
//   - (zero, ErrKeyMissing)    if rawKey is empty
//   - (zero, ErrKeyInvalid)    if no matching row
//   - (zero, ErrKeyExpired)    if the matched row has expires_at in the past
//
// Successful lookups bump api_keys.last_used_at asynchronously — failure
// to update is non-fatal (the DB might be read-only or slow; the auth
// answer doesn't depend on it).
func (v *AuthVerifier) Verify(ctx context.Context, rawKey string) (domain.Identity, error) {
	if rawKey == "" {
		return domain.Identity{}, port.ErrKeyMissing
	}

	hash := hashKey(rawKey)
	if id, ok := v.cache.get(hash); ok {
		return id, nil
	}

	var (
		keyID, tenantID uuid.UUID
		scopesJSON      []byte
		expiresAt       sql.NullTime
		tenantStatus    string
	)
	err := v.db.QueryRowContext(ctx, `
		SELECT k.id, k.tenant_id, k.scopes, k.expires_at, t.status
		FROM api_keys k
		JOIN tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = $1
	`, hash).Scan(&keyID, &tenantID, &scopesJSON, &expiresAt, &tenantStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Identity{}, port.ErrKeyInvalid
	}
	if err != nil {
		return domain.Identity{}, fmt.Errorf("auth verify: %w", err)
	}
	if tenantStatus == string(domain.TenantStatusSuspended) {
		return domain.Identity{}, port.ErrKeyRevoked
	}
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return domain.Identity{}, port.ErrKeyExpired
	}

	var scopes []string
	if len(scopesJSON) > 0 {
		if err := json.Unmarshal(scopesJSON, &scopes); err != nil {
			return domain.Identity{}, fmt.Errorf("auth verify: scopes JSON: %w", err)
		}
	}

	identity := domain.Identity{
		TenantID: tenantID,
		KeyID:    keyID,
	}
	identity.Scopes = scopes
	if expiresAt.Valid {
		t := expiresAt.Time
		identity.ExpiresAt = &t
	}

	v.cache.put(hash, identity)
	go v.touchLastUsed(keyID) // fire-and-forget

	return identity, nil
}

// EnsureLegacyTenant idempotently provisions the legacy tenant and,
// optionally, a key with the given raw value. Called on startup so a
// fresh database picks up an immediately-usable credential without
// requiring a separate provisioning step.
//
// rawKey may be empty: in that case only the tenant row is ensured;
// nothing is granted a key. The middleware then runs in dev mode, where
// requests are authenticated as the legacy tenant without a credential
// (see middleware/auth.go).
func (v *AuthVerifier) EnsureLegacyTenant(ctx context.Context, rawKey string) error {
	_, err := v.db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, status)
		VALUES ($1, 'legacy', 'active')
		ON CONFLICT (id) DO NOTHING
	`, domain.LegacyTenantID)
	if err != nil {
		return fmt.Errorf("ensure legacy tenant: %w", err)
	}
	if rawKey == "" {
		return nil
	}
	hash := hashKey(rawKey)
	scopes, _ := json.Marshal([]string{"admin:*"})
	_, err = v.db.ExecContext(ctx, `
		INSERT INTO api_keys (tenant_id, key_hash, name, scopes)
		VALUES ($1, $2, 'API_KEY env (legacy)', $3::JSONB)
		ON CONFLICT (key_hash) DO NOTHING
	`, domain.LegacyTenantID, hash, scopes)
	if err != nil {
		return fmt.Errorf("ensure legacy api key: %w", err)
	}
	return nil
}

// CreateAPIKey provisions a new key for tenantID with the given scopes
// and returns (id, rawKey, hash). The raw key is returned exactly once;
// callers must surface it to the operator immediately. Useful for
// admin endpoints and for test setup.
func (v *AuthVerifier) CreateAPIKey(ctx context.Context, tenantID uuid.UUID, name string, scopes []string) (id uuid.UUID, rawKey, keyHash string, err error) {
	rawKey = uuid.New().String() + "-" + uuid.New().String() // ~256 bits of entropy
	keyHash = hashKey(rawKey)
	scopesJSON, _ := json.Marshal(scopes)
	err = v.db.QueryRowContext(ctx, `
		INSERT INTO api_keys (tenant_id, key_hash, name, scopes)
		VALUES ($1, $2, $3, $4::JSONB)
		RETURNING id
	`, tenantID, keyHash, name, scopesJSON).Scan(&id)
	if err != nil {
		return uuid.Nil, "", "", fmt.Errorf("create api key: %w", err)
	}
	return id, rawKey, keyHash, nil
}

// CreateTenant provisions a new tenant row. Convenience for admin
// flows and test setup.
func (v *AuthVerifier) CreateTenant(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := v.db.QueryRowContext(ctx, `
		INSERT INTO tenants (name)
		VALUES ($1)
		RETURNING id
	`, name).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create tenant: %w", err)
	}
	return id, nil
}

// InvalidateCache drops the in-memory cache. Used when a key is
// revoked / scopes change and we don't want to wait for the TTL.
func (v *AuthVerifier) InvalidateCache() {
	v.cache.clear()
}

func (v *AuthVerifier) touchLastUsed(keyID uuid.UUID) {
	// Use a fresh background ctx — we don't want a request deadline to
	// cancel the bookkeeping write. Bounded by an explicit timeout so a
	// stuck DB doesn't leak goroutines.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = v.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
}

func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// keyCache is a tiny TTL cache for hash → Identity lookups. Reads
// dominate writes (a key is verified many times per second; rotation
// is rare), so a single mutex is fine. The cache is bounded by the
// number of distinct keys in active use, which for B2B at tens of
// tenants is small.
type keyCache struct {
	ttl   time.Duration
	mu    sync.RWMutex
	items map[string]cachedIdentity
}

type cachedIdentity struct {
	identity domain.Identity
	expires  time.Time
}

func newKeyCache(ttl time.Duration) *keyCache {
	return &keyCache{ttl: ttl, items: make(map[string]cachedIdentity)}
}

func (c *keyCache) get(hash string) (domain.Identity, bool) {
	if c.ttl <= 0 {
		return domain.Identity{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if entry, ok := c.items[hash]; ok && time.Now().Before(entry.expires) {
		return entry.identity, true
	}
	return domain.Identity{}, false
}

func (c *keyCache) put(hash string, id domain.Identity) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[hash] = cachedIdentity{identity: id, expires: time.Now().Add(c.ttl)}
}

func (c *keyCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]cachedIdentity)
}

// Compile-time interface satisfaction check.
var _ port.AuthVerifier = (*AuthVerifier)(nil)
