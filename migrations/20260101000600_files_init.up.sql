-- ---------------------------------------------------------------------------
-- files: metadata (blobs live in local/S3 storage)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS files (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    original_name VARCHAR(255) NOT NULL,
    storage_path VARCHAR(512) NOT NULL UNIQUE,
    mime_type VARCHAR(127) NOT NULL,
    size BIGINT NOT NULL CHECK (size >= 0),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    CONSTRAINT fk_files_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE RESTRICT
);

-- Deliberately NOT partial: it serves ListFilesByUserCursor for both
-- include_deleted values under a generic plan, ListFilesByUserID and
-- CountFilesByUserID (deleted_at is a cheap heap filter), and the
-- fk_files_tenant_user check when a users row is deleted or re-keyed.
CREATE INDEX IF NOT EXISTS idx_files_tenant_user_created
    ON files (tenant_id, user_id, created_at DESC, id DESC);
-- AdminListFilesCursor / AdminListFiles / AdminFileStats.
CREATE INDEX IF NOT EXISTS idx_files_tenant_created_at_id
    ON files (tenant_id, created_at DESC, id DESC);

DROP TRIGGER IF EXISTS trigger_files_updated_at ON files;
CREATE TRIGGER trigger_files_updated_at
    BEFORE UPDATE ON files
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();
