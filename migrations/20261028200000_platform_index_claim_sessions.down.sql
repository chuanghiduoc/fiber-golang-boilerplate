-- owner: platform

DROP INDEX IF EXISTS idx_jobs_claim_kind;

DROP INDEX IF EXISTS idx_sessions_tenant_user_created_at;
CREATE INDEX idx_sessions_tenant_user_created_at
    ON sessions (tenant_id, user_id, created_at DESC);
