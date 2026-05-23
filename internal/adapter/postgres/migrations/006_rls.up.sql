-- 006_rls.up.sql
-- Multi-tenancy Phase 2: Row-Level Security as defence in depth.
--
-- Phase 1 (migration 005) added a tenant_id column to every domain table
-- and the application layer filters every query with
-- `WHERE tenant_id = $`. RLS is the safety net for that filter: even if
-- a future query is written without the WHERE clause, Postgres refuses
-- to return cross-tenant rows.
--
-- See DESIGN.md §3 for the design contract and the dual-mode policy
-- (tenant_isolation OR system_bypass).

-- app_current_tenant() returns the request-scoped tenant UUID, or NULL
-- if app.tenant_id is not set on the session. NULLIF turns the
-- "missing setting" empty string into a true NULL so the cast to UUID
-- doesn't fail; the policy treats NULL = NULL as false (no rows).
CREATE OR REPLACE FUNCTION app_current_tenant() RETURNS uuid AS $$
    SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid;
$$ LANGUAGE sql STABLE;

-- app_system_mode() returns true when the maintenance / worker
-- bypass is engaged. Set via `SET LOCAL app.system_mode = '1'` from
-- the postgres.withSystemTx helper. Used for global ops like Cleanup,
-- Dequeue, Stats — work that intentionally spans tenants.
CREATE OR REPLACE FUNCTION app_system_mode() RETURNS boolean AS $$
    SELECT current_setting('app.system_mode', true) = '1';
$$ LANGUAGE sql STABLE;

-- Helper that builds the standard dual-mode policy on each tenant
-- table. Multiple permissive policies are OR'd by Postgres, so a row
-- is visible if EITHER the tenant matches OR system mode is engaged.

-- tags
ALTER TABLE tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE tags FORCE ROW LEVEL SECURITY; -- applies to table owner too
CREATE POLICY tags_tenant_isolation ON tags
    FOR ALL
    USING      (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
CREATE POLICY tags_system_bypass ON tags
    FOR ALL
    USING      (app_system_mode())
    WITH CHECK (app_system_mode());

-- media
ALTER TABLE media ENABLE ROW LEVEL SECURITY;
ALTER TABLE media FORCE ROW LEVEL SECURITY;
CREATE POLICY media_tenant_isolation ON media
    FOR ALL
    USING      (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
CREATE POLICY media_system_bypass ON media
    FOR ALL
    USING      (app_system_mode())
    WITH CHECK (app_system_mode());

-- media_tags
ALTER TABLE media_tags ENABLE ROW LEVEL SECURITY;
ALTER TABLE media_tags FORCE ROW LEVEL SECURITY;
CREATE POLICY media_tags_tenant_isolation ON media_tags
    FOR ALL
    USING      (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
CREATE POLICY media_tags_system_bypass ON media_tags
    FOR ALL
    USING      (app_system_mode())
    WITH CHECK (app_system_mode());

-- idempotency_keys
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idem_tenant_isolation ON idempotency_keys
    FOR ALL
    USING      (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
CREATE POLICY idem_system_bypass ON idempotency_keys
    FOR ALL
    USING      (app_system_mode())
    WITH CHECK (app_system_mode());

-- jobs
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY jobs_tenant_isolation ON jobs
    FOR ALL
    USING      (tenant_id = app_current_tenant())
    WITH CHECK (tenant_id = app_current_tenant());
CREATE POLICY jobs_system_bypass ON jobs
    FOR ALL
    USING      (app_system_mode())
    WITH CHECK (app_system_mode());

-- Intentionally NOT RLS-protected:
--   tenants, api_keys — shared infrastructure read by the auth verifier
--     BEFORE a tenant context exists. These rows are only ever managed
--     by the system itself, never exposed to tenant callers.
