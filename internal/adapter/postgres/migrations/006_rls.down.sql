-- 006_rls.down.sql
-- Reverse of 006_rls.up.sql. Drops the policies, FORCE flag, and the
-- helper functions; leaves the data and tenant_id columns in place
-- (those belong to migration 005).

ALTER TABLE jobs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE jobs DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS jobs_tenant_isolation ON jobs;
DROP POLICY IF EXISTS jobs_system_bypass ON jobs;

ALTER TABLE idempotency_keys NO FORCE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idem_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS idem_system_bypass ON idempotency_keys;

ALTER TABLE media_tags NO FORCE ROW LEVEL SECURITY;
ALTER TABLE media_tags DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS media_tags_tenant_isolation ON media_tags;
DROP POLICY IF EXISTS media_tags_system_bypass ON media_tags;

ALTER TABLE media NO FORCE ROW LEVEL SECURITY;
ALTER TABLE media DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS media_tenant_isolation ON media;
DROP POLICY IF EXISTS media_system_bypass ON media;

ALTER TABLE tags NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tags DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tags_tenant_isolation ON tags;
DROP POLICY IF EXISTS tags_system_bypass ON tags;

DROP FUNCTION IF EXISTS app_system_mode();
DROP FUNCTION IF EXISTS app_current_tenant();
