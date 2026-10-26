#!/bin/sh
# PostgreSQL entrypoint hook: create the non-superuser runtime roles (the
# tenant-scoped app role and the dedicated BYPASSRLS role).
# Runs once, when the data volume is first initialized, as POSTGRES_USER.
set -eu

psql -v ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  -v app_user="${APP_DB_USERNAME:-app}" \
  -v app_password="${APP_DB_PASSWORD:-app-password}" \
  -v bypass_user="${APP_DB_BYPASS_USERNAME:-app_bypass}" \
  -v bypass_password="${APP_DB_BYPASS_PASSWORD:-app-bypass-password}" \
  -v owner="$POSTGRES_USER" \
  -v schema="${DB_SCHEMA:-public}" \
  -f /opt/app/database-roles.sql
