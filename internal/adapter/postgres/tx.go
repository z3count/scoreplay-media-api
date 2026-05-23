package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/port"
)

// withTenantTx runs fn inside a tenant-scoped transaction.
//
// The flow:
//   1. Extract the TenantID from ctx (set by the auth middleware).
//   2. BEGIN a transaction.
//   3. SET LOCAL app.tenant_id = <uuid> — enables the tenant_isolation
//      RLS policy on every table in the request's path.
//   4. Invoke fn with the tx + tenant id. fn uses tx for all queries.
//   5. COMMIT on success, ROLLBACK on error (deferred).
//
// Why a transaction wrapper rather than `*sql.Conn` + plain SET:
//   - `SET LOCAL` auto-resets at COMMIT/ROLLBACK; no risk of one
//     request's tenant leaking into another via a pooled connection.
//   - Existing repo methods that already need atomicity (e.g.,
//     MediaRepo.Create's media + media_tags insert) collapse cleanly
//     into the same transaction.
//   - Read-only queries inside a tx are essentially free in Postgres;
//     the overhead vs. a bare query is microseconds.
//
// The closure receives the resolved tenantID so SQL bodies can keep
// their explicit `WHERE tenant_id = $X` filters as defence in depth.
// RLS alone is the safety net; the WHERE clauses are the primary
// enforcement.
func withTenantTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx, tenantID uuid.UUID) error) error {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tenant tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// pq doesn't parameterise SET — substitute via quote_literal at the
	// SQL boundary. tenantID is a uuid.UUID stringified via .String(),
	// which produces a canonical safe form; we still quote it for
	// clarity.
	if _, err := tx.ExecContext(ctx, `SET LOCAL app.tenant_id = '`+tenantID.String()+`'`); err != nil {
		return fmt.Errorf("set tenant in tx: %w", err)
	}

	if err := fn(tx, tenantID); err != nil {
		return err
	}
	return tx.Commit()
}

// withSystemTx runs fn inside a "system mode" transaction that bypasses
// per-tenant RLS. Used for genuinely cross-tenant operations:
//
//   - The worker pool's Dequeue (picks any tenant's job).
//   - Cleanup goroutines for expired idempotency keys and old jobs.
//   - Stats sampling for the saturation metrics (counts across all
//     tenants).
//
// fn must understand that it can see and modify rows belonging to any
// tenant. Be deliberate — system mode is the same blast radius as a
// missed RLS policy.
//
// The mechanism: SET LOCAL app.system_mode = '1'; the system_bypass
// policy on every RLS-protected table allows the operation.
func withSystemTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin system tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `SET LOCAL app.system_mode = '1'`); err != nil {
		return fmt.Errorf("set system mode in tx: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
