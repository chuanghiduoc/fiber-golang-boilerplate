-- Creates the non-superuser role the API and worker connect as.
-- Run once as the schema owner, before the first cmd/migrate:
--
--   psql -v app_user=app -v app_password='...' -v owner=postgres -v schema=public \
--     -f deploy/postgres/database-roles.sql
--
-- FORCE row-level security subjects the table owner too, but the owner can
-- run ALTER TABLE ... NO FORCE ROW LEVEL SECURITY, so the API must not use the
-- owner either: it connects as this role, which owns no table and is not a
-- member of a superuser or BYPASSRLS role. cmd/api refuses to start in
-- production with any of those. TRUNCATE is deliberately not granted because
-- it ignores row-level security.

SELECT format('CREATE ROLE %I LOGIN', :'app_user')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app_user')\gexec

ALTER ROLE :"app_user" WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD :'app_password';

GRANT USAGE ON SCHEMA :"schema" TO :"app_user";
-- No TRUNCATE: it ignores row-level security, so granting it to the runtime
-- role would let any bug erase every tenant's rows. Only the schema owner
-- (cmd/migrate) may truncate.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA :"schema" TO :"app_user";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA :"schema" TO :"app_user";
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA :"schema" TO :"app_user";

-- Tables created by later migrations inherit the same grants.
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO :"app_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT USAGE, SELECT ON SEQUENCES TO :"app_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT EXECUTE ON FUNCTIONS TO :"app_user";
