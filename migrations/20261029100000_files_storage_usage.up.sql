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

-- The counters are maintained once per statement from the transition tables,
-- with one upsert per affected (tenant, user). A row-level trigger would run
-- one upsert per file instead, and a bulk insert for a single user then walks
-- a chain of dead tuple versions that grows with every row: 10k rows already
-- took minutes on PostgreSQL 18. Only the owning user's counter moves, so two
-- users uploading concurrently never contend for the same row, and a counter
-- update is an additive upsert that stays correct under concurrency.

-- A user who gained files gets their summed size added to their counter.
CREATE OR REPLACE FUNCTION files_storage_usage_after_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
    SELECT tenant_id, user_id, SUM(size)::bigint
    FROM new_rows
    GROUP BY tenant_id, user_id
    ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
    SET bytes = files_storage_usage.bytes + EXCLUDED.bytes;
    RETURN NULL;
END;
$$;

-- Deleted bytes leave the counter, and a counter that reaches zero disappears:
-- erasure and tenant purge must not leave a zero-byte user identifier behind.
-- A missing counter for a non-zero delete is corruption, not a normal path; a
-- zero-byte delete that finds no row is simply a counter already cleaned up.
CREATE OR REPLACE FUNCTION files_storage_usage_after_delete() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    missing BIGINT;
BEGIN
    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(size)::bigint AS bytes
        FROM old_rows
        GROUP BY tenant_id, user_id
    )
    SELECT count(*) INTO missing
    FROM deltas d
    LEFT JOIN files_storage_usage u
        ON u.tenant_id = d.tenant_id AND u.user_id = d.user_id
    WHERE u.tenant_id IS NULL AND d.bytes > 0;
    IF missing > 0 THEN
        RAISE EXCEPTION 'files_storage_usage row missing for % deleted (tenant, user) pair(s)', missing;
    END IF;

    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(size)::bigint AS bytes
        FROM old_rows
        GROUP BY tenant_id, user_id
    )
    UPDATE files_storage_usage u
    SET bytes = u.bytes - d.bytes
    FROM deltas d
    WHERE u.tenant_id = d.tenant_id AND u.user_id = d.user_id;

    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(size)::bigint AS bytes
        FROM old_rows
        GROUP BY tenant_id, user_id
    )
    DELETE FROM files_storage_usage u
    USING deltas d
    WHERE u.tenant_id = d.tenant_id
      AND u.user_id = d.user_id
      AND u.bytes = 0;
    RETURN NULL;
END;
$$;

-- An update moves the delta only: a size change adjusts one counter, and a
-- file moved to another tenant or user subtracts from the old counter and adds
-- to the new one. Combinations whose delta nets out to zero (for example a
-- zero-byte file moving between tenants) need no row.
CREATE OR REPLACE FUNCTION files_storage_usage_after_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    missing BIGINT;
BEGIN
    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(delta)::bigint AS bytes
        FROM (
            SELECT tenant_id, user_id, -size AS delta FROM old_rows
            UNION ALL
            SELECT tenant_id, user_id, size AS delta FROM new_rows
        ) AS moved
        GROUP BY tenant_id, user_id
        HAVING SUM(delta) <> 0
    )
    SELECT count(*) INTO missing
    FROM deltas d
    LEFT JOIN files_storage_usage u
        ON u.tenant_id = d.tenant_id AND u.user_id = d.user_id
    WHERE u.tenant_id IS NULL AND d.bytes < 0;
    IF missing > 0 THEN
        RAISE EXCEPTION 'files_storage_usage row missing for % updated (tenant, user) pair(s)', missing;
    END IF;

    -- Growing counters may need a row; a negative delta must never be proposed
    -- to the CHECK constraint, so shrinking ones are a plain UPDATE instead.
    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(delta)::bigint AS bytes
        FROM (
            SELECT tenant_id, user_id, -size AS delta FROM old_rows
            UNION ALL
            SELECT tenant_id, user_id, size AS delta FROM new_rows
        ) AS moved
        GROUP BY tenant_id, user_id
        HAVING SUM(delta) > 0
    )
    INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
    SELECT tenant_id, user_id, bytes FROM deltas
    ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
    SET bytes = files_storage_usage.bytes + EXCLUDED.bytes;

    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(delta)::bigint AS bytes
        FROM (
            SELECT tenant_id, user_id, -size AS delta FROM old_rows
            UNION ALL
            SELECT tenant_id, user_id, size AS delta FROM new_rows
        ) AS moved
        GROUP BY tenant_id, user_id
        HAVING SUM(delta) < 0
    )
    UPDATE files_storage_usage u
    SET bytes = u.bytes + d.bytes
    FROM deltas d
    WHERE u.tenant_id = d.tenant_id AND u.user_id = d.user_id;

    WITH deltas AS (
        SELECT tenant_id, user_id, SUM(delta)::bigint AS bytes
        FROM (
            SELECT tenant_id, user_id, -size AS delta FROM old_rows
            UNION ALL
            SELECT tenant_id, user_id, size AS delta FROM new_rows
        ) AS moved
        GROUP BY tenant_id, user_id
        HAVING SUM(delta) <> 0
    )
    DELETE FROM files_storage_usage u
    USING deltas d
    WHERE u.tenant_id = d.tenant_id
      AND u.user_id = d.user_id
      AND u.bytes = 0;
    RETURN NULL;
END;
$$;

DO $$
DECLARE
    target_schema TEXT := current_schema();
BEGIN
    IF target_schema IS NULL THEN
        RAISE EXCEPTION 'cannot pin files_storage_usage trigger search_path: current_schema() is NULL';
    END IF;
    EXECUTE format(
        'ALTER FUNCTION %I.files_storage_usage_after_insert() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.files_storage_usage_after_delete() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.files_storage_usage_after_update() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
END;
$$;

CREATE TRIGGER trigger_files_storage_usage_insert
    AFTER INSERT ON files
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT
    EXECUTE FUNCTION files_storage_usage_after_insert();

CREATE TRIGGER trigger_files_storage_usage_update
    AFTER UPDATE ON files
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT
    EXECUTE FUNCTION files_storage_usage_after_update();

CREATE TRIGGER trigger_files_storage_usage_delete
    AFTER DELETE ON files
    REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT
    EXECUTE FUNCTION files_storage_usage_after_delete();

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