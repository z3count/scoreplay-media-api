package postgres

import (
	"context"
	"database/sql"
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
// Returns found=false if the key doesn't exist or has expired.
func (s *IdempotencyStore) Get(ctx context.Context, key string) (int, []byte, bool, error) {
	var statusCode int
	var body []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT status_code, response FROM idempotency_keys
		 WHERE key = $1 AND expires_at > now()`,
		key,
	).Scan(&statusCode, &body)

	if err == sql.ErrNoRows {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	return statusCode, body, true, nil
}

// Set stores a response for the given idempotency key.
// Uses INSERT ... ON CONFLICT to handle race conditions where two requests
// with the same key arrive simultaneously — the first one wins.
func (s *IdempotencyStore) Set(ctx context.Context, key string, statusCode int, body []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, status_code, response)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO NOTHING`,
		key, statusCode, body,
	)
	return err
}

// Cleanup removes expired idempotency keys.
// Should be called periodically (e.g. every hour) to prevent table bloat.
func (s *IdempotencyStore) Cleanup(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE expires_at <= now()`,
	)
	return err
}
