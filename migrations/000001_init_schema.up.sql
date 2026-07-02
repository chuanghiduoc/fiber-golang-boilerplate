-- Initial schema: users, files, and token tables.
-- This boilerplate ships a single init migration; add new versioned migrations
-- (make migrate-create name=...) for changes on top of this baseline.

-- Shared trigger to keep updated_at current on UPDATE.
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Users
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) UNIQUE NOT NULL,
    password_hash VARCHAR(255),
    name VARCHAR(255) NOT NULL,
    role VARCHAR(50) NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    google_id VARCHAR(255) UNIQUE,
    auth_provider VARCHAR(50) NOT NULL DEFAULT 'local' CHECK (auth_provider IN ('local', 'google')),
    email_verified_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_users_email ON users(email);
CREATE INDEX idx_users_deleted_at ON users(deleted_at) WHERE deleted_at IS NULL;
-- Supports cursor pagination ordered by (created_at, id) DESC.
CREATE INDEX idx_users_created_at_id ON users(created_at DESC, id DESC);

CREATE TRIGGER trigger_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- Files
CREATE TABLE IF NOT EXISTS files (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_name VARCHAR(255) NOT NULL,
    storage_path VARCHAR(512) NOT NULL UNIQUE,
    mime_type VARCHAR(127) NOT NULL,
    size BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_files_user_id ON files(user_id);
CREATE INDEX idx_files_deleted_at ON files(deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX idx_files_user_id_deleted_at ON files(user_id) WHERE deleted_at IS NULL;
-- Cursor pagination: per-user list and admin-wide list ordered by (created_at, id) DESC.
CREATE INDEX idx_files_user_created ON files(user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_files_created_at_id ON files(created_at DESC, id DESC);

-- Refresh tokens (token column stores a SHA-256 hash, not the plaintext)
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_refresh_tokens_token ON refresh_tokens(token);
CREATE INDEX idx_refresh_tokens_user_id ON refresh_tokens(user_id);

-- Password reset tokens (token column stores a SHA-256 hash)
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_password_reset_tokens_token ON password_reset_tokens(token);
CREATE INDEX idx_password_reset_tokens_user_id ON password_reset_tokens(user_id);

-- Email verification tokens (token column stores a SHA-256 hash)
CREATE TABLE IF NOT EXISTS email_verification_tokens (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_email_verification_tokens_token ON email_verification_tokens(token);
CREATE INDEX idx_email_verification_tokens_user_id ON email_verification_tokens(user_id);
