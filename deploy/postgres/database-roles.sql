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
-- row-level security. TEMP is revoked on the database as well: a runtime role
-- that can create temp tables could shadow the schema's real table inside a
-- trigger function, so the runtime roles must not be able to create them.

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

-- TEMP is a search_path shadowing primitive: a runtime role that can create
-- temp tables could shadow an unqualified table name inside a trigger
-- function for one statement. The functions pin search_path as well; removing
-- the primitive is the second half of the fix.
--
-- New databases grant TEMP to PUBLIC by default, so revoking it from the two
-- runtime roles alone would leave them with the PUBLIC grant. Revoke it from
-- PUBLIC on this dedicated application database (the owner keeps its explicit
-- grant; a superuser ignores ACLs anyway).
SELECT format('REVOKE TEMP ON DATABASE %I FROM PUBLIC', current_database())\gexec
SELECT format('REVOKE TEMP ON DATABASE %I FROM %I, %I',
              current_database(), :'app_user', :'bypass_user')\gexec
SELECT format('GRANT TEMP ON DATABASE %I TO %I', current_database(), :'owner')\gexec

-- golang-migrate owns schema_migrations. The default privileges below would
-- otherwise let a runtime role rewrite the recorded version (skipping or
-- replaying DDL). This script normally runs before the first migration, so
-- the table may not exist yet: the guarded REVOKE is a no-op then, and
-- cmd/migrate revokes the same privileges after every migration run.
SELECT format('REVOKE ALL ON TABLE %I.schema_migrations FROM %I, %I',
              :'schema', :'app_user', :'bypass_user')
WHERE to_regclass(format('%I.schema_migrations', :'schema')) IS NOT NULL\gexec

-- Tables created by later migrations inherit the same grants.
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO :"app_user", :"bypass_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT USAGE, SELECT ON SEQUENCES TO :"app_user", :"bypass_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT EXECUTE ON FUNCTIONS TO :"app_user", :"bypass_user";
