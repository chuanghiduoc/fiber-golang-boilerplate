-- Fresh-install baseline, split per owner (ADR 0005). The baseline files run
-- in timestamp order: shared functions, identity, platform tables, then the
-- modules in foreign-key dependency order. Later changes are new
-- <UTC timestamp>_<owner>_<purpose> migrations; never edit a shipped file once
-- a database has applied it.
--
-- Conventions for every baseline file:
--   * one owner per table; the file name names the owner;
--   * active-row uniqueness uses partial indexes (soft delete friendly);
--   * cursor pagination is backed by (created_at DESC, id DESC) indexes;
--   * timestamps are NOT NULL so pagination cannot hide rows;
--   * every tenant-owned row carries tenant_id and tenant-leading indexes;
--   * surrogate keys are BIGINT GENERATED ALWAYS AS IDENTITY, so an explicit
--     id needs OVERRIDING SYSTEM VALUE (only the default tenant seed uses it);
--   * a cross-module child of users references (tenant_id, user_id) with
--     ON DELETE RESTRICT: users are soft-deleted and another module's rows
--     must never vanish through a cascade it does not own.

CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
