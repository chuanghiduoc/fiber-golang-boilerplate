-- owner: platform

-- Keyset pagination orders by (created_at DESC, id DESC). The extra id column
-- lets PostgreSQL satisfy the full ORDER BY from the index instead of sorting
-- the page when several sessions share a timestamp.
DROP INDEX IF EXISTS idx_sessions_tenant_user_created_at;
CREATE INDEX idx_sessions_tenant_user_created_at
    ON sessions (tenant_id, user_id, created_at DESC, id DESC);

-- Dedicated workers claim a kind-filtered due set (kind = ANY(...)); this
-- index keeps that scan ordered by run_at, id without touching the heap.
CREATE INDEX idx_jobs_claim_kind
    ON jobs (kind, run_at, id)
    WHERE status = 'queued';
