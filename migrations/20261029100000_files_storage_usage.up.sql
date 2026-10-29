-- owner: files

-- Per-membership storage counters for upload quotas. There is deliberately no
-- tenant aggregate row: a tenant-wide write would serialize every uploader and
-- would deadlock with deployments that only enable the per-user quota. Tenant
-- usage is SUM(bytes) over these rows, read only when that quota is enabled.
-- Both quotas count trashed rows because their blobs still occupy storage, so
-- only INSERT/DELETE/size changes move the counters. Soft-delete and restore
-- leave them unchanged.
CREATE TABLE files_storage_usage (
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    bytes BIGINT NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    CONSTRAINT fk_files_storage_usage_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    -- A counter row belongs to one membership; removing the membership (for
    -- example during tenant purge or a completed erasure) must not leave a
    -- per-user identifier behind.
    CONSTRAINT fk_files_storage_usage_tenant_user
        FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE,
    CONSTRAINT uq_files_storage_usage_tenant_user
        UNIQUE (tenant_id, user_id) INCLUDE (bytes)
);

-- Backfill under a table lock so no upload can slip between the counter
-- snapshot and the trigger installation. SHARE ROW EXCLUSIVE still allows
-- reads. files has FORCE ROW LEVEL SECURITY, which applies to the owner too,
-- so the backfill must temporarily clear FORCE for this transaction. The
-- migration role is the schema owner (it created the table and trigger), but
-- it is not assumed to be a superuser or to have BYPASSRLS.
LOCK TABLE files IN SHARE ROW EXCLUSIVE MODE;
ALTER TABLE files NO FORCE ROW LEVEL SECURITY;

INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
SELECT tenant_id, user_id, COALESCE(SUM(size), 0)::bigint
FROM files
GROUP BY tenant_id, user_id;

ALTER TABLE files FORCE ROW LEVEL SECURITY;

-- Only the owning user's counter moves. Keeping the tenant aggregate out of
-- the trigger is what lets two users upload concurrently when only the user
-- quota is enabled.
CREATE OR REPLACE FUNCTION files_track_storage_usage() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    delta BIGINT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
        VALUES (NEW.tenant_id, NEW.user_id, NEW.size)
        ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
        SET bytes = files_storage_usage.bytes + EXCLUDED.bytes;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        -- Remove the counter row when its last byte is released. This keeps
        -- erasure and tenant purge from leaving a zero-byte user identifier
        -- behind. A missing row is corruption, not a normal delete path.
        DELETE FROM files_storage_usage
        WHERE tenant_id = OLD.tenant_id
          AND user_id = OLD.user_id
          AND bytes = OLD.size;
        IF NOT FOUND THEN
            -- The first zero-byte file may already have removed the
            -- zero-byte counter row. Further zero-byte deletes are still
            -- valid and must not surface as corruption.
            IF OLD.size = 0 THEN
                RETURN OLD;
            END IF;
            UPDATE files_storage_usage
            SET bytes = bytes - OLD.size
            WHERE tenant_id = OLD.tenant_id AND user_id = OLD.user_id;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'files_storage_usage row missing for tenant % user %',
                    OLD.tenant_id, OLD.user_id;
            END IF;
        END IF;
        RETURN OLD;
    ELSE
        IF NEW.tenant_id <> OLD.tenant_id OR NEW.user_id <> OLD.user_id THEN
            INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
            VALUES (OLD.tenant_id, OLD.user_id, -OLD.size)
            ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
            SET bytes = files_storage_usage.bytes + EXCLUDED.bytes;
            INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
            VALUES (NEW.tenant_id, NEW.user_id, NEW.size)
            ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
            SET bytes = files_storage_usage.bytes + EXCLUDED.bytes;
            RETURN NEW;
        END IF;
        delta := NEW.size - OLD.size;
        IF delta = 0 THEN
            RETURN NEW;
        END IF;
        INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
        VALUES (NEW.tenant_id, NEW.user_id, delta)
        ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
        SET bytes = files_storage_usage.bytes + EXCLUDED.bytes;
        RETURN NEW;
    END IF;
END;
$$;

DO $$
DECLARE
    target_schema TEXT := current_schema();
BEGIN
    IF target_schema IS NULL THEN
        RAISE EXCEPTION 'cannot pin files_track_storage_usage search_path: current_schema() is NULL';
    END IF;
    EXECUTE format(
        'ALTER FUNCTION %I.files_track_storage_usage() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
END;
$$;

CREATE TRIGGER trigger_files_storage_usage
    AFTER INSERT OR DELETE OR UPDATE OF tenant_id, user_id, size ON files
    FOR EACH ROW
    EXECUTE FUNCTION files_track_storage_usage();

ALTER TABLE files_storage_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE files_storage_usage FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON files_storage_usage
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

-- AdminFileStats is an index-only count/sum over live rows. Quota checks use
-- the per-user counter table and do not add another files aggregate index.
CREATE INDEX idx_files_tenant_live_size
    ON files (tenant_id) INCLUDE (size)
    WHERE deleted_at IS NULL;
