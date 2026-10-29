-- owner: files

DROP INDEX IF EXISTS idx_files_tenant_live_size;

DROP TRIGGER IF EXISTS trigger_files_storage_usage ON files;
DROP FUNCTION IF EXISTS files_track_storage_usage();
DROP TABLE IF EXISTS files_storage_usage;
