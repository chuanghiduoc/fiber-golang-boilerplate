-- ---------------------------------------------------------------------------
-- notifications: per-user in-app feed and channel preferences
--
-- A notification row is the in-app delivery. Email delivery is a durable job
-- enqueued in the same transaction as the row, so a queue failure cannot leave
-- a half-delivered notification behind. Users may disable either channel per
-- type; a missing preference row means both channels are enabled.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS notifications (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    type VARCHAR(100) NOT NULL,
    title VARCHAR(255) NOT NULL,
    body TEXT NOT NULL,
    data JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_notifications_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

-- The non-partial index also serves the fk_notifications_tenant_user check.
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_user_created_at_id
    ON notifications (tenant_id, user_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_user_unread
    ON notifications (tenant_id, user_id, created_at DESC, id DESC)
    WHERE read_at IS NULL;

DROP TRIGGER IF EXISTS trigger_notifications_updated_at ON notifications;
CREATE TRIGGER trigger_notifications_updated_at
    BEFORE UPDATE ON notifications
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS notification_preferences (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    type VARCHAR(100) NOT NULL,
    channel VARCHAR(20) NOT NULL CHECK (channel IN ('in_app', 'email')),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_notification_preferences_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

-- Also serves the fk_notification_preferences_tenant_user check.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_preferences_unique
    ON notification_preferences (tenant_id, user_id, type, channel);

DROP TRIGGER IF EXISTS trigger_notification_preferences_updated_at ON notification_preferences;
CREATE TRIGGER trigger_notification_preferences_updated_at
    BEFORE UPDATE ON notification_preferences
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();
