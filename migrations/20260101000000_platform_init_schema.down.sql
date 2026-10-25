-- Drops everything the init migration creates, owners in reverse order.

-- owner: webhooks
DROP TABLE IF EXISTS webhook_inbound_events;
DROP TABLE IF EXISTS webhook_inbound_sources;
DROP TABLE IF EXISTS webhook_delivery_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS webhook_endpoint_secrets;
DROP TABLE IF EXISTS webhook_endpoint_subscriptions;
DROP TABLE IF EXISTS webhook_endpoints;

-- owner: notifications
DROP TABLE IF EXISTS notification_preferences;
DROP TABLE IF EXISTS notifications;

-- owner: features
DROP TABLE IF EXISTS feature_flag_targets;
DROP TABLE IF EXISTS feature_flags;

-- owner: files
DROP TABLE IF EXISTS files;

-- owner: apikeys
DROP TABLE IF EXISTS api_key_scopes;
DROP TABLE IF EXISTS api_keys;

-- owner: platform
-- Dropping the table drops its triggers; the trigger functions go after it.
DROP TABLE IF EXISTS audit_events;
DROP FUNCTION IF EXISTS audit_events_append_only();
DROP FUNCTION IF EXISTS audit_events_completion_guard();

-- owner: platform
DROP TABLE IF EXISTS scheduler_runs;
DROP TABLE IF EXISTS scheduler_leases;

-- owner: platform
DROP TABLE IF EXISTS jobs;

-- owner: identity
DROP TABLE IF EXISTS mfa_recovery_codes;
DROP TABLE IF EXISTS mfa_factors;
DROP TABLE IF EXISTS data_exports;
DROP TABLE IF EXISTS platform_role_assignments;
DROP TABLE IF EXISTS membership_roles;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS permissions;
DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS memberships;
-- Dropping memberships removes the last trigger that depends on these
-- functions.
DROP FUNCTION IF EXISTS protect_last_owner();
DROP FUNCTION IF EXISTS validate_membership_role();
DROP FUNCTION IF EXISTS validate_platform_role();
DROP FUNCTION IF EXISTS validate_role_permission();
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS user_passwords;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenant_settings;
DROP TABLE IF EXISTS tenants;

-- owner: platform
-- Each tenant-owned table above drops its own tenant_isolation policy with it.
DROP FUNCTION IF EXISTS app_tenant_visible(BIGINT);
DROP FUNCTION IF EXISTS update_updated_at();
