-- Identity/session hardening.
--
-- 1. Access tokens carry a token_version. Bumping it invalidates every
--    previously issued access token for that user (password change/reset,
--    ban, role change).
-- 2. Emails are stored lowercase and unique only among active users. This
--    makes lookups case-insensitive and allows an email to be reused after
--    the previous account is soft-deleted.
-- 3. created_at/updated_at are NOT NULL so cursor pagination cannot silently
--    hide rows with NULL sort keys.

-- Fail fast instead of blocking auth traffic indefinitely on a busy table.
SET lock_timeout = '5s';

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS token_version INTEGER NOT NULL DEFAULT 0;
-- Password-reset links issued before this migration (or before the last
-- password/email change) must not remain usable.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS password_changed_at TIMESTAMP WITH TIME ZONE;
UPDATE users SET password_changed_at = NOW() WHERE password_changed_at IS NULL;

-- Backfill timestamps before tightening NOT NULL constraints.
UPDATE users SET created_at = NOW() WHERE created_at IS NULL;
UPDATE users SET updated_at = NOW() WHERE updated_at IS NULL;
UPDATE files SET created_at = NOW() WHERE created_at IS NULL;
UPDATE refresh_tokens SET created_at = NOW() WHERE created_at IS NULL;
UPDATE password_reset_tokens SET created_at = NOW() WHERE created_at IS NULL;
UPDATE email_verification_tokens SET created_at = NOW() WHERE created_at IS NULL;

ALTER TABLE users ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE users ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE files ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE refresh_tokens ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE password_reset_tokens ALTER COLUMN created_at SET NOT NULL;
ALTER TABLE email_verification_tokens ALTER COLUMN created_at SET NOT NULL;

-- Replace the original case-sensitive/global unique constraints with partial,
-- case-insensitive indexes over active users only. Drop them before
-- lowercasing so a soft-deleted row reusing an email cannot trip the old
-- global constraint.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_key;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_google_id_key;

-- Refuse to silently merge ACTIVE accounts that differ only by case. Duplicates
-- involving soft-deleted rows are allowed: email reuse after deletion is a
-- supported flow.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM users
        WHERE deleted_at IS NULL
        GROUP BY lower(email)
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'active users contain duplicate emails differing only by case; resolve them before applying this migration';
    END IF;
END
$$;

UPDATE users SET email = lower(email) WHERE email <> lower(email);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_lower_active
    ON users (lower(email))
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_google_id_active
    ON users (google_id)
    WHERE deleted_at IS NULL AND google_id IS NOT NULL;

-- The original exact-case email index is unused now that lookups are
-- case-insensitive. Add an index for the last-admin count/guard.
DROP INDEX IF EXISTS idx_users_email;
CREATE INDEX IF NOT EXISTS idx_users_role_active
    ON users (role)
    WHERE deleted_at IS NULL;

-- Bind refresh tokens to the user's token version. A refresh that races with a
-- password change/reset can then be rejected when it tries to mint a new
-- token, instead of surviving the revocation DELETE.
ALTER TABLE refresh_tokens
    ADD COLUMN IF NOT EXISTS token_version INTEGER NOT NULL DEFAULT 0;
UPDATE refresh_tokens rt
SET token_version = u.token_version
FROM users u
WHERE rt.user_id = u.id;

-- Exactly one reset/verification token per user. The application uses an
-- upsert, so concurrent requests replace rather than duplicate tokens.
DELETE FROM password_reset_tokens a
USING password_reset_tokens b
WHERE a.user_id = b.user_id AND a.id < b.id;

DELETE FROM email_verification_tokens a
USING email_verification_tokens b
WHERE a.user_id = b.user_id AND a.id < b.id;

-- 000001 created non-unique indexes with these names. Drop them first;
-- CREATE ... IF NOT EXISTS would otherwise silently keep the non-unique ones.
DROP INDEX IF EXISTS idx_password_reset_tokens_user_id;
DROP INDEX IF EXISTS idx_email_verification_tokens_user_id;

CREATE UNIQUE INDEX idx_password_reset_tokens_user_id
    ON password_reset_tokens (user_id);
CREATE UNIQUE INDEX idx_email_verification_tokens_user_id
    ON email_verification_tokens (user_id);

-- Verification links must prove ownership of the address they verify, not
-- just the account ID (an old link must not verify a newly changed address).
ALTER TABLE email_verification_tokens
    ADD COLUMN IF NOT EXISTS email VARCHAR(255);

DELETE FROM email_verification_tokens t
WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = t.user_id);

UPDATE email_verification_tokens t
SET email = u.email
FROM users u
WHERE t.user_id = u.id AND t.email IS NULL;

ALTER TABLE email_verification_tokens ALTER COLUMN email SET NOT NULL;

-- Defense in depth for last-admin safety. Application checks return a friendly
-- 409, but two concurrent requests could still both pass the count check. This
-- trigger locks the remaining admin rows, so at least one active admin always
-- survives.
CREATE OR REPLACE FUNCTION protect_last_admin()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.role = 'admin'
       AND OLD.deleted_at IS NULL
       AND (TG_OP = 'DELETE' OR NEW.role <> 'admin' OR NEW.deleted_at IS NOT NULL) THEN
        PERFORM 1
        FROM users
        WHERE role = 'admin' AND deleted_at IS NULL AND id <> OLD.id
        FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'cannot remove the last active admin';
        END IF;
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_protect_last_admin ON users;
CREATE TRIGGER trigger_protect_last_admin
    BEFORE UPDATE OF role, deleted_at OR DELETE ON users
    FOR EACH ROW
    EXECUTE FUNCTION protect_last_admin();
