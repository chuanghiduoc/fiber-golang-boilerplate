-- ---------------------------------------------------------------------------
-- platform: scheduler leader lease and run ledger
--
-- The lease is a single row per scheduler group; the holder renews it until it
-- exits or loses the lease. scheduler_runs is the unique ledger that prevents
-- duplicate enqueues for the same schedule slot, even across leaders.
-- These tables deliberately have no foreign keys: the scheduler is a platform
-- component and its rows must survive tenant/module lifecycle changes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS scheduler_leases (
    name TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    heartbeat_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS scheduler_runs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    schedule TEXT NOT NULL,
    scheduled_for TIMESTAMP WITH TIME ZONE NOT NULL,
    status TEXT NOT NULL DEFAULT 'enqueued'
        CHECK (status IN ('enqueued', 'skipped', 'failed')),
    job_id BIGINT,
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_runs_schedule_slot
    ON scheduler_runs (schedule, scheduled_for);
CREATE INDEX IF NOT EXISTS idx_scheduler_runs_created_at
    ON scheduler_runs (created_at);
