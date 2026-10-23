DO $$
BEGIN
    IF to_regclass('users') IS NOT NULL THEN
        DROP TRIGGER IF EXISTS trigger_protect_last_admin ON users;
        DROP TRIGGER IF EXISTS trigger_users_updated_at ON users;
    END IF;
END $$;

DROP FUNCTION IF EXISTS protect_last_admin();

DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS users;

DROP FUNCTION IF EXISTS update_updated_at();
