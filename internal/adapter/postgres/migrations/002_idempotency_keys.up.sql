-- Idempotency key store for safe request retries.
--
-- When a client sends an Idempotency-Key header, the server stores
-- the response and replays it on subsequent requests with the same key.
-- This prevents duplicate media uploads when retries occur (network
-- timeouts, mobile connectivity changes, load balancer retries).
--
-- Keys expire after 24 hours and should be cleaned up periodically.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key         TEXT        PRIMARY KEY,
    status_code INT         NOT NULL,
    response    BYTEA       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours'
);

-- Index for efficient cleanup of expired keys.
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires_at
    ON idempotency_keys (expires_at);
