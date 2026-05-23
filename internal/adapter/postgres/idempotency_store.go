package postgres

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
)

// IdempotencyStore implements port.IdempotencyStore using PostgreSQL.
//
// Cached responses are stored in the idempotency_keys table with a 24-hour
// TTL. Expired keys are excluded from Get queries and removed by Cleanup.
type IdempotencyStore struct {
	db *sql.DB
}

// NewIdempotencyStore creates a new PostgreSQL-backed idempotency store.
func NewIdempotencyStore(db *sql.DB) *IdempotencyStore {
	return &IdempotencyStore{db: db}
}

// Get retrieves a cached response for the given idempotency key.
// Returns found=false if the key doesn't exist or has expired. The
// lookup is tenant-scoped — tenant A's idempotency key X is a different
// entry from tenant B's X.
func (s *IdempotencyStore) Get(ctx context.Context, key string) (int, []byte, bool, error) {
	var statusCode int
	var body []byte
	var found bool
	err := withTenantTx(ctx, s.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		err := tx.QueryRowContext(ctx,
			`SELECT status_code, response FROM idempotency_keys
			 WHERE tenant_id = $1 AND key = $2 AND expires_at > now()`,
			tenantID, key,
		).Scan(&statusCode, &body)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return 0, nil, false, err
	}
	return statusCode, body, found, nil
}

// Set stores a response for the given idempotency key, tenant-scoped.
// Uses INSERT ... ON CONFLICT to handle race conditions where two requests
// with the same key arrive simultaneously — the first one wins.
func (s *IdempotencyStore) Set(ctx context.Context, key string, statusCode int, body []byte) error {
	return withTenantTx(ctx, s.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (tenant_id, key, status_code, response)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, key) DO NOTHING`,
			tenantID, key, statusCode, body,
		)
		return err
	})
}

// Cleanup removes expired idempotency keys across all tenants.
// Should be called periodically (e.g. every hour) to prevent table bloat.
// System-mode tx bypasses RLS — this is a cross-tenant maintenance op,
// not a tenant-scoped query.
func (s *IdempotencyStore) Cleanup(ctx context.Context) error {
	return withSystemTx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM idempotency_keys WHERE expires_at <= now()`,
		)
		return err
	})
}
