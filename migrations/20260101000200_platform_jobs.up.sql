-- ---------------------------------------------------------------------------
-- platform: durable jobs / outbox
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS jobs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    kind TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- W3C traceparent captured when the job was enqueued. The worker uses it
    -- for a span link only; it never affects tenant scoping or retries.
    trace_parent TEXT,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'dead')),
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    -- idempotency_key deduplicates producer retries (event fan-out, scheduled
    -- slots). NULL means the job is not deduplicated.
    idempotency_key TEXT,
    run_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    locked_at TIMESTAMP WITH TIME ZONE,
    locked_by TEXT,
    -- lease_expires_at is the owning worker's lease deadline, written as
    -- now() + that worker's lease on claim and on every heartbeat. Keeping it
    -- per row means a worker configured with a short lease cannot reclaim a
    -- healthy job held by a worker with a longer lease.
    lease_expires_at TIMESTAMP WITH TIME ZONE,
    last_error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- A running row without a deadline could never be reclaimed after its
    -- worker crashed, so the database refuses it.
    CONSTRAINT jobs_running_has_lease
        CHECK (status <> 'running' OR lease_expires_at IS NOT NULL),
    -- Exactly the running rows have an owner: the claim sets locked_by and
    -- every transition out of running (succeed, fail, release, lease-expiry
    -- requeue) clears it, so ownership fences can never match a queued or
    -- terminal row.
    CONSTRAINT jobs_running_has_owner
        CHECK ((status = 'running') = (locked_by IS NOT NULL))
);

-- claimJobsSQL is a cross-tenant pass: the due CTE (status = 'queued',
-- run_at <= now()) uses this. Claim and purge are global by design, so there
-- is no tenant-leading status/kind index; the only tenant-scoped lookup is the
-- idempotency key.
CREATE INDEX IF NOT EXISTS idx_jobs_status_run_at ON jobs (status, run_at);
-- The lease-expiry CTE scans running rows by deadline
-- (status = 'running' AND lease_expires_at < now()); the partial predicate
-- keeps the index as small as the in-flight set.
CREATE INDEX IF NOT EXISTS idx_jobs_running_lease_expires_at
    ON jobs (lease_expires_at)
    WHERE status = 'running';
-- Keys are scoped by tenant because callers may use user-provided keys.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_tenant_idempotency_key
    ON jobs (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- Supports the batched retention purge, which orders terminal rows by age.
-- The predicate matches purgeTerminalJobsSQL so the planner can use it.
CREATE INDEX IF NOT EXISTS idx_jobs_terminal_updated_at
    ON jobs (updated_at)
    WHERE status IN ('succeeded', 'dead');
