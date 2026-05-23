-- Replace the single-column created_at index with a compound (created_at DESC, id DESC)
-- index to support keyset/cursor-based pagination.
--
-- Cursor pagination uses the row comparison `(created_at, id) < ($1, $2)`
-- with `ORDER BY created_at DESC, id DESC`. PostgreSQL can satisfy both the
-- ORDER BY and the inequality with a single backward range scan over this
-- compound index — constant time regardless of how deep the page is.

DROP INDEX IF EXISTS idx_media_created_at;

CREATE INDEX IF NOT EXISTS idx_media_cursor
    ON media (created_at DESC, id DESC);
