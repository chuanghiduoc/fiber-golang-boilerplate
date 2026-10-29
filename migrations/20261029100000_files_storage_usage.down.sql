-- owner: files

DROP INDEX IF EXISTS idx_files_tenant_live_size;

DROP TRIGGER IF EXISTS trigger_files_storage_usage_insert ON files;
DROP TRIGGER IF EXISTS trigger_files_storage_usage_update ON files;
DROP TRIGGER IF EXISTS trigger_files_storage_usage_delete ON files;
DROP FUNCTION IF EXISTS files_storage_usage_after_insert();
DROP FUNCTION IF EXISTS files_storage_usage_after_update();
DROP FUNCTION IF EXISTS files_storage_usage_after_delete();
DROP TABLE IF EXISTS files_storage_usage;