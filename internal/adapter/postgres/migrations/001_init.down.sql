-- 001_init.down.sql
-- Rolls back the initial schema creation.
-- Order matters: drop junction table first to avoid FK violations.

DROP TABLE IF EXISTS media_tags;
DROP TABLE IF EXISTS media;
DROP TABLE IF EXISTS tags;
