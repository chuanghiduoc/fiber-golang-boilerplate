-- Creates the two non-superuser roles the API, worker, and seed connect as.
-- Run once as the schema owner, before the first cmd/migrate:
--
--   psql -v app_user=app -v app_password='...' \
--     -v bypass_user=app_bypass -v bypass_password='...' \
--     -v owner=postgres -v schema=public \
--     -f deploy/postgres/database-roles.sql
--
-- FORCE row-level security subjects the table owner too, but the owner can
-- run ALTER TABLE ... NO FORCE ROW LEVEL SECURITY, so the API must not use the
-- owner either: it connects as app_user for tenant-scoped work and
-- bypass_user for deliberate cross-tenant work.
--
--   * app_user has no BYPASSRLS, owns no table, and is not a member of
--     bypass_user (so SET ROLE is impossible). cmd/api refuses to start in
--     production otherwise.
--   * bypass_user has BYPASSRLS so global job claim, lease recovery,
--     retention purges, and catalog sync can span tenants, but it is not a
--     superuser and owns nothing.
--
-- TRUNCATE is deliberately not granted to either role because it ignores
-- row-level security.

SELECT format('CREATE ROLE %I LOGIN', :'app_user')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app_user')\gexec
SELECT format('CREATE ROLE %I LOGIN', :'bypass_user')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'bypass_user')\gexec

ALTER ROLE :"app_user" WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD :'app_password';
ALTER ROLE :"bypass_user" WITH LOGIN NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD :'bypass_password';

-- The normal role must not be able to SET ROLE into the bypass role. This is
-- a no-op on a fresh install and repairs an older grant on an existing one.
REVOKE :"bypass_user" FROM :"app_user";

GRANT USAGE ON SCHEMA :"schema" TO :"app_user", :"bypass_user";
-- No TRUNCATE: it ignores row-level security, so granting it to a runtime
-- role would let any bug erase every tenant's rows. Only the schema owner
-- (cmd/migrate) may truncate.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA :"schema" TO :"app_user", :"bypass_user";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA :"schema" TO :"app_user", :"bypass_user";
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA :"schema" TO :"app_user", :"bypass_user";

-- Tables created by later migrations inherit the same grants.
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO :"app_user", :"bypass_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT USAGE, SELECT ON SEQUENCES TO :"app_user", :"bypass_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT EXECUTE ON FUNCTIONS TO :"app_user", :"bypass_user";
