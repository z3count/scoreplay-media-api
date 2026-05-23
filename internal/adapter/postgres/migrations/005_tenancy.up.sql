-- 005_tenancy.up.sql
-- Multi-tenant evolution (Phase 1): tenant_id columns + tenants/api_keys
-- tables, with a "legacy" tenant pre-provisioned so existing rows have a
-- valid foreign key target and existing API_KEY values keep working.
--
-- See DESIGN.md §3 for the design contract.
--
-- Phase 2 (deferred): Postgres Row-Level Security as defence-in-depth.
-- That needs `SET LOCAL app.tenant_id = …` per request, which means
-- routing every query through `*sql.Conn` / `*sql.Tx`. The WHERE clauses
-- + the cross-tenant integration test deliver the correctness guarantee
-- in the meantime.

-- The "legacy" tenant id is fixed (00000000-0000-0000-0000-000000000001)
-- so the migration is reproducible. It's the bucket that the existing
-- single-API_KEY world lives in until an operator splits real tenants
-- out via SQL or the admin endpoints.

CREATE TABLE tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active', 'suspended')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- SHA-256 hex of the raw key. The raw key is shown to the operator
    -- once at creation time and never stored.
    key_hash     TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    -- Authorisation: array of scope strings such as "media:write" or
    -- "admin:*". Stored as JSONB so it can grow without a schema change.
    scopes       JSONB NOT NULL DEFAULT '[]'::JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);

CREATE INDEX idx_api_keys_tenant ON api_keys(tenant_id);

-- Pre-provision the legacy tenant. Idempotent via the fixed UUID so
-- re-running the migration is safe.
INSERT INTO tenants (id, name, status)
VALUES ('00000000-0000-0000-0000-000000000001', 'legacy', 'active')
ON CONFLICT (id) DO NOTHING;

-- Add tenant_id to every domain table with the legacy tenant as the
-- backfill default. The default is dropped at the end of the migration
-- so future writes must be explicit.

ALTER TABLE tags
    ADD COLUMN tenant_id UUID NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES tenants(id) ON DELETE CASCADE;
-- tags.name was globally UNIQUE. With tenancy it must be unique per
-- tenant: different tenants get to use the same tag name.
ALTER TABLE tags DROP CONSTRAINT tags_name_key;
ALTER TABLE tags ADD CONSTRAINT tags_tenant_name_key UNIQUE (tenant_id, name);
ALTER TABLE tags ALTER COLUMN tenant_id DROP DEFAULT;

ALTER TABLE media
    ADD COLUMN tenant_id UUID NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE media ALTER COLUMN tenant_id DROP DEFAULT;

-- Drop the old cursor index and replace with one prefixed by tenant_id
-- so the keyset scan stays bounded per tenant.
DROP INDEX IF EXISTS idx_media_cursor;
CREATE INDEX idx_media_cursor ON media (tenant_id, created_at DESC, id DESC);

-- media_tags is a junction table; both foreign keys already carry their
-- own tenant ownership. Adding a denormalised tenant_id here lets us
-- enforce "you can only tag your own media with your own tags" with a
-- single check constraint, and lets joins prune by tenant earlier.
ALTER TABLE media_tags
    ADD COLUMN tenant_id UUID NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES tenants(id) ON DELETE CASCADE;
ALTER TABLE media_tags ALTER COLUMN tenant_id DROP DEFAULT;

ALTER TABLE idempotency_keys
    ADD COLUMN tenant_id UUID NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES tenants(id) ON DELETE CASCADE;
-- The PK was just (key). With tenancy it must be (tenant_id, key) so
-- two tenants can share a key value without colliding.
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys ADD PRIMARY KEY (tenant_id, key);
ALTER TABLE idempotency_keys ALTER COLUMN tenant_id DROP DEFAULT;

ALTER TABLE jobs
    ADD COLUMN tenant_id UUID NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000001'
        REFERENCES tenants(id) ON DELETE CASCADE;
-- The dequeue index is filtered to pending jobs. Keep the partial
-- nature but lead with tenant_id so workers in a multi-tenant world
-- can also be tenant-scoped if needed.
DROP INDEX IF EXISTS idx_jobs_dequeue;
CREATE INDEX idx_jobs_dequeue
    ON jobs (tenant_id, scheduled_at)
    WHERE status = 'pending';
ALTER TABLE jobs ALTER COLUMN tenant_id DROP DEFAULT;
