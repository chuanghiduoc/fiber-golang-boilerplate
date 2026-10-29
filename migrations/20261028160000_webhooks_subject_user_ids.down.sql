DROP INDEX IF EXISTS idx_webhook_events_subject_user_ids;
ALTER TABLE webhook_events DROP COLUMN IF EXISTS subject_user_ids;
