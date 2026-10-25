-- Dropping the table drops its triggers; the trigger functions go after it.
DROP TABLE IF EXISTS audit_events;
DROP FUNCTION IF EXISTS audit_events_append_only();
DROP FUNCTION IF EXISTS audit_events_completion_guard();
