-- 005_tenancy.down.sql
-- Reverse of 005_tenancy.up.sql. Drops the tenant_id columns, restores
-- the original global uniqueness constraints, and removes the tenants
-- + api_keys tables.

DROP INDEX IF EXISTS idx_jobs_dequeue;
ALTER TABLE jobs DROP COLUMN IF EXISTS tenant_id;
CREATE INDEX IF NOT EXISTS idx_jobs_dequeue
    ON jobs (scheduled_at)
    WHERE status = 'pending';

ALTER TABLE idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_pkey;
ALTER TABLE idempotency_keys DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE idempotency_keys ADD PRIMARY KEY (key);

ALTER TABLE media_tags DROP COLUMN IF EXISTS tenant_id;

DROP INDEX IF EXISTS idx_media_cursor;
ALTER TABLE media DROP COLUMN IF EXISTS tenant_id;
CREATE INDEX IF NOT EXISTS idx_media_cursor
    ON media (created_at DESC, id DESC);

ALTER TABLE tags DROP CONSTRAINT IF EXISTS tags_tenant_name_key;
ALTER TABLE tags DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE tags ADD CONSTRAINT tags_name_key UNIQUE (name);

DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS tenants;
