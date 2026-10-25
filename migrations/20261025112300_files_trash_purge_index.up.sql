-- files: trash retention (files.trash.purge).
--
-- The purge is a deliberate cross-tenant maintenance pass that selects
-- soft-deleted rows by age, so this index leads with deleted_at instead of
-- tenant_id. It is partial: live rows (deleted_at IS NULL, the vast majority)
-- are not indexed, so uploads pay nothing for it. CONCURRENTLY avoids blocking
-- writes on a live files table; golang-migrate runs this single statement
-- outside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_files_deleted_at
    ON files (deleted_at)
    WHERE deleted_at IS NOT NULL;
