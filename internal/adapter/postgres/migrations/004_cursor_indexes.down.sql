DROP INDEX IF EXISTS idx_media_cursor;

CREATE INDEX IF NOT EXISTS idx_media_created_at ON media (created_at DESC);
