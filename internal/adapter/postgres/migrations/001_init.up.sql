-- 001_init.up.sql
-- Creates the foundational schema for the media management API.
--
-- Design decisions:
--   - UUID primary keys: prevent sequential ID enumeration (security) and are
--     safe for distributed/multi-instance environments.
--   - tags.name is UNIQUE: enables idempotent tag creation (INSERT ... ON CONFLICT).
--   - media_tags junction table: models the many-to-many relationship between
--     media and tags, enabling future "find media by tag" queries efficiently.
--   - Indexes on media_tags.tag_id: the composite PK (media_id, tag_id) already
--     covers "find tags for media". This index covers the reverse direction.
--   - media.media_type CHECK constraint: database-level validation ensures only
--     'image' or 'video' types are stored, even if application-level validation
--     is bypassed.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE tags (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE media (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    media_type    TEXT NOT NULL CHECK (media_type IN ('image', 'video')),
    file_path     TEXT NOT NULL,
    original_name TEXT NOT NULL,
    file_size     BIGINT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE media_tags (
    media_id UUID NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    tag_id   UUID NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (media_id, tag_id)
);

-- Index for "find all media for a given tag" queries.
-- The composite PK already serves "find all tags for a given media".
CREATE INDEX idx_media_tags_tag_id ON media_tags(tag_id);

-- Index for listing media ordered by creation date (default listing order).
CREATE INDEX idx_media_created_at ON media(created_at DESC);
