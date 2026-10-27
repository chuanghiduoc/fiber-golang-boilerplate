-- Drops everything the init migration creates, owners in reverse order.

-- owner: webhooks
DROP TABLE webhook_inbound_events;
DROP TABLE webhook_inbound_source_secrets;
DROP TABLE webhook_inbound_sources;
DROP TABLE webhook_delivery_attempts;
DROP TABLE webhook_deliveries;
DROP TABLE webhook_events;
DROP TABLE webhook_endpoint_secrets;
DROP TABLE webhook_endpoint_subscriptions;
DROP TABLE webhook_endpoints;

-- owner: notifications
DROP TABLE notification_preferences;
DROP TABLE notifications;

-- owner: features
DROP TABLE feature_flag_targets;
DROP TABLE feature_flags;

-- owner: files
DROP TABLE files;

-- owner: apikeys
DROP TABLE api_key_scopes;
DROP TABLE api_keys;

-- owner: platform
-- Dropping the partitioned table drops its default and monthly partitions and
-- every trigger on them; the trigger functions go after it.
DROP TABLE audit_events;
DROP FUNCTION audit_events_append_only();
DROP FUNCTION audit_events_completion_guard();

-- owner: platform
DROP TABLE scheduler_runs;
DROP TABLE scheduler_leases;

-- owner: platform
DROP TABLE jobs;

-- owner: identity
DROP TABLE webauthn_credentials;
DROP TABLE mfa_recovery_codes;
DROP TABLE mfa_factors;
DROP TABLE data_exports;
DROP TABLE platform_role_assignments;
DROP TABLE membership_roles;
DROP TABLE role_permissions;
DROP TABLE invitations;
DROP TABLE roles;
DROP TABLE permissions;
DROP TABLE email_verification_tokens;
DROP TABLE password_reset_tokens;
DROP TABLE refresh_tokens;
DROP TABLE sessions;
DROP TABLE memberships;
-- Dropping memberships removes the last trigger that depends on these
-- functions.
DROP FUNCTION protect_last_owner();
DROP FUNCTION validate_invitation_role();
DROP FUNCTION validate_membership_role();
DROP FUNCTION validate_platform_role();
DROP FUNCTION validate_role_permission();
DROP TABLE user_identities;
DROP TABLE user_passwords;
DROP TABLE users;
DROP TABLE tenant_settings;
DROP TABLE tenants;

-- owner: platform
-- Each tenant-owned table above drops its own tenant_isolation policy with it.
DROP FUNCTION app_tenant_visible(UUID);
DROP FUNCTION update_updated_at();
