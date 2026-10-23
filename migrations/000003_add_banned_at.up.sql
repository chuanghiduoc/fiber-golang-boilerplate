-- Distinguish an administrator ban from a self-service account deletion.
-- Banned emails must not be reusable via registration or Google login.
--
-- This is a separate migration because 000002 has already shipped; existing
-- installations at version 2 apply this file on their next startup.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS banned_at TIMESTAMP WITH TIME ZONE;
