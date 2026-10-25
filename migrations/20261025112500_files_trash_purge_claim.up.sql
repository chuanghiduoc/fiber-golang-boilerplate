-- files: purge claim. Set before the blob is deleted; restore requires NULL.
ALTER TABLE files ADD COLUMN IF NOT EXISTS purge_started_at TIMESTAMP WITH TIME ZONE;
