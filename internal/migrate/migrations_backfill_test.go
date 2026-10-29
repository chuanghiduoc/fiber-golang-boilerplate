package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackfillMigrationsClearForcedRLS guards migrations that rewrite tenant
// rows. files and webhook_events use FORCE ROW LEVEL SECURITY, so the schema
// owner sees no rows unless the migration temporarily clears FORCE. Without
// the paired CLEAR/RESTORE statements the backfills silently produce empty
// counters or lose legacy webhook subjects on an upgrade.
func TestBackfillMigrationsClearForcedRLS(t *testing.T) {
	cases := []struct {
		name      string
		file      string
		table     string
		backfill  string
		afterward string
	}{
		{
			name:      "webhook subject backfill",
			file:      "20261028160000_webhooks_subject_user_ids.up.sql",
			table:     "webhook_events",
			backfill:  "UPDATE webhook_events AS e",
			afterward: "CREATE INDEX idx_webhook_events_subject_user_ids",
		},
		{
			name:      "files storage backfill",
			file:      "20261029100000_files_storage_usage.up.sql",
			table:     "files",
			backfill:  "INSERT INTO files_storage_usage",
			afterward: "CREATE OR REPLACE FUNCTION files_storage_usage_after_insert",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", tc.file))
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			source := string(raw)
			noForce := "ALTER TABLE " + tc.table + " NO FORCE ROW LEVEL SECURITY;"
			force := "ALTER TABLE " + tc.table + " FORCE ROW LEVEL SECURITY;"
			noForceAt := strings.Index(source, noForce)
			backfillAt := strings.Index(source, tc.backfill)
			forceAt := strings.Index(source, force)
			afterwardAt := strings.Index(source, tc.afterward)
			if noForceAt < 0 || backfillAt < 0 || forceAt < 0 || afterwardAt < 0 {
				t.Fatalf("migration is missing the FORCE RLS guard or backfill anchor: no-force=%d backfill=%d force=%d after=%d",
					noForceAt, backfillAt, forceAt, afterwardAt)
			}
			if noForceAt >= backfillAt || backfillAt >= forceAt || forceAt >= afterwardAt {
				t.Fatalf("FORCE RLS guard must wrap the backfill (no-force=%d backfill=%d force=%d after=%d)",
					noForceAt, backfillAt, forceAt, afterwardAt)
			}
		})
	}
}

// TestWebhookSubjectBackfillSkipsNilAndGuardsCast keeps the legacy backfill
// aligned with payloadSubjectUserIDs: malformed JSON values must not abort the
// migration and the all-zero UUID is not a subject.
func TestWebhookSubjectBackfillSkipsNilAndGuardsCast(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261028160000_webhooks_subject_user_ids.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	source := string(raw)
	for _, want := range []string{
		"CASE WHEN pg_input_is_valid(entry.value, 'uuid') THEN entry.value::uuid END",
		"candidate.subject_id <> '00000000-0000-0000-0000-000000000000'::uuid",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("webhook subject backfill is missing %q", want)
		}
	}
}
