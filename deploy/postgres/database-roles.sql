-- Creates the non-superuser role the API and worker connect as.
-- Run once as the schema owner, before the first cmd/migrate:
--
--   psql -v app_user=app -v app_password='...' -v owner=postgres -v schema=public \
--     -f deploy/postgres/database-roles.sql
--
-- FORCE row-level security subjects the table owner too, so the owner role may
-- run migrations but must not be used by the API: superusers and roles with
-- BYPASSRLS ignore every policy, and cmd/api refuses to start in production
-- with one.

SELECT format('CREATE ROLE %I LOGIN', :'app_user')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app_user')\gexec

ALTER ROLE :"app_user" WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD :'app_password';

GRANT USAGE ON SCHEMA :"schema" TO :"app_user";
GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA :"schema" TO :"app_user";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA :"schema" TO :"app_user";
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA :"schema" TO :"app_user";

-- Tables created by later migrations inherit the same grants.
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLES TO :"app_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT USAGE, SELECT ON SEQUENCES TO :"app_user";
ALTER DEFAULT PRIVILEGES FOR ROLE :"owner" IN SCHEMA :"schema"
    GRANT EXECUTE ON FUNCTIONS TO :"app_user";
