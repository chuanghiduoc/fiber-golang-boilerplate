-- webhook_events previously attributed an event to a user only through the
-- actor and two payload keys. That misses events whose subject is another user
-- (for example an admin deleting someone else's file) and couples privacy
-- erasure to each producer's payload shape.
--
-- subject_user_ids is the durable, explicit set of users whose data the event
-- concerns. The dispatcher writes it when the event is first persisted; the
-- privacy exporter/eraser reads it with a GIN index.
ALTER TABLE webhook_events
    ADD COLUMN subject_user_ids UUID[] NOT NULL DEFAULT '{}'::uuid[];

-- Backfill existing rows from the actor and the top-level user-id fields used
-- by the events shipped before this column existed. pg_input_is_valid keeps a
-- malformed legacy payload from aborting the migration.
--
-- webhook_events uses FORCE ROW LEVEL SECURITY, which also applies to its
-- owner. The migration role may be a non-superuser schema owner, so clear FORCE
-- only for this transaction; otherwise the UPDATE silently sees no rows.
-- A failure rolls the transaction back and restores the original state.
ALTER TABLE webhook_events NO FORCE ROW LEVEL SECURITY;

UPDATE webhook_events AS e
SET subject_user_ids = COALESCE((
    SELECT array_agg(DISTINCT candidate.subject_id ORDER BY candidate.subject_id)
    FROM (
        SELECT e.actor_id AS subject_id
        WHERE e.actor_type = 'user' AND e.actor_id IS NOT NULL
        UNION ALL
        -- CASE makes the cast lazy; do not rely on the planner evaluating the
        -- pg_input_is_valid filter before the SELECT expression.
        SELECT CASE WHEN pg_input_is_valid(entry.value, 'uuid') THEN entry.value::uuid END
        FROM jsonb_each_text(
            CASE
                WHEN jsonb_typeof(e.payload) = 'object' THEN e.payload
                ELSE '{}'::jsonb
            END
        ) AS entry
        WHERE entry.key IN (
            'userId', 'ownerId', 'accountId', 'memberId', 'subjectId', 'targetUserId'
        )
          AND entry.value <> ''
          AND pg_input_is_valid(entry.value, 'uuid')
    ) AS candidate
    WHERE candidate.subject_id IS NOT NULL
      -- The dispatcher treats the all-zero UUID as "no subject"; keep the
      -- backfill identical so a legacy payload cannot create a bogus subject.
      AND candidate.subject_id <> '00000000-0000-0000-0000-000000000000'::uuid
), '{}'::uuid[]);

ALTER TABLE webhook_events FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_webhook_events_subject_user_ids
    ON webhook_events USING GIN (subject_user_ids);

COMMENT ON COLUMN webhook_events.subject_user_ids IS
    'Users whose data this outbound event concerns; used by privacy export/erasure.';
