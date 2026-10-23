DROP TRIGGER IF EXISTS trigger_protect_last_admin ON users;
DROP FUNCTION IF EXISTS protect_last_admin();

DROP INDEX IF EXISTS idx_email_verification_tokens_user_id;
DROP INDEX IF EXISTS idx_password_reset_tokens_user_id;

-- Restore the non-unique indexes from 000001.
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_user_id
    ON password_reset_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_user_id
    ON email_verification_tokens (user_id);

ALTER TABLE email_verification_tokens DROP COLUMN IF EXISTS email;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS token_version;

DROP INDEX IF EXISTS idx_users_role_active;
DROP INDEX IF EXISTS idx_users_google_id_active;
DROP INDEX IF EXISTS idx_users_email_lower_active;

-- Rolling back to a global unique constraint is impossible while a
-- soft-deleted account shares an email/google_id with an active one (a state
-- the up migration explicitly permits). Fail with an actionable message
-- instead of a raw duplicate-key error; never delete user rows automatically.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users GROUP BY email HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'cannot roll back: duplicate emails exist; deduplicate users manually before running the down migration';
    END IF;
    IF EXISTS (
        SELECT 1 FROM users
        WHERE google_id IS NOT NULL
        GROUP BY google_id
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot roll back: duplicate google_id values exist; deduplicate users manually before running the down migration';
    END IF;
END
$$;

ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
ALTER TABLE users ADD CONSTRAINT users_google_id_key UNIQUE (google_id);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);

ALTER TABLE email_verification_tokens ALTER COLUMN created_at DROP NOT NULL;
ALTER TABLE password_reset_tokens ALTER COLUMN created_at DROP NOT NULL;
ALTER TABLE refresh_tokens ALTER COLUMN created_at DROP NOT NULL;
ALTER TABLE files ALTER COLUMN created_at DROP NOT NULL;
ALTER TABLE users ALTER COLUMN updated_at DROP NOT NULL;
ALTER TABLE users ALTER COLUMN created_at DROP NOT NULL;

ALTER TABLE users DROP COLUMN IF EXISTS banned_at;
ALTER TABLE users DROP COLUMN IF EXISTS password_changed_at;
ALTER TABLE users DROP COLUMN IF EXISTS token_version;
