package migrate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditPartitionsAtUsesUTCAndRollsOverYear(t *testing.T) {
	partitions := auditPartitionsAt(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))

	if len(partitions) != 2 {
		t.Fatalf("partitions = %d, want 2", len(partitions))
	}
	if partitions[0].name != "audit_events_202612" {
		t.Fatalf("current partition = %q, want audit_events_202612", partitions[0].name)
	}
	if partitions[1].name != "audit_events_202701" {
		t.Fatalf("next partition = %q, want audit_events_202701", partitions[1].name)
	}
	if got := partitions[0].start.Format("2006-01-02"); got != "2026-12-01" {
		t.Fatalf("current start = %s, want 2026-12-01", got)
	}
	if got := partitions[1].end.Format("2006-01-02"); got != "2027-02-01" {
		t.Fatalf("next end = %s, want 2027-02-01", got)
	}
}

func TestAuditPartitionsAtConvertsToUTC(t *testing.T) {
	// 2027-01-01 00:30 at UTC+7 is still December in UTC.
	ict := time.FixedZone("ICT", 7*60*60)
	partitions := auditPartitionsAt(time.Date(2027, 1, 1, 0, 30, 0, 0, ict))

	if partitions[0].name != "audit_events_202612" {
		t.Fatalf("current partition = %q, want audit_events_202612", partitions[0].name)
	}
	if partitions[1].name != "audit_events_202701" {
		t.Fatalf("next partition = %q, want audit_events_202701", partitions[1].name)
	}
}

func TestWriteSnapshotFileReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.snapshot.sql")
	if err := os.WriteFile(path, []byte("old snapshot\n"), 0o600); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	want := []byte("new snapshot\n")
	if err := writeSnapshotFile(path, want); err != nil {
		t.Fatalf("writeSnapshotFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("snapshot content = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "schema.snapshot.sql" {
		t.Fatalf("temporary file was not cleaned up: %v", entries)
	}
}
