package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

type fakeRow struct {
	scan func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error {
	return r.scan(dest...)
}

type fakeMigrationQueryer struct {
	rows  []pgx.Row
	calls int
}

func (q *fakeMigrationQueryer) QueryRow(context.Context, string, ...any) pgx.Row {
	if q.calls >= len(q.rows) {
		panic("unexpected QueryRow call")
	}
	row := q.rows[q.calls]
	q.calls++
	return row
}

func boolRow(value bool) pgx.Row {
	return fakeRow{scan: func(dest ...any) error {
		*dest[0].(*bool) = value
		return nil
	}}
}

func versionRow(version int64, dirty bool) pgx.Row {
	return fakeRow{scan: func(dest ...any) error {
		*dest[0].(*int64) = version
		*dest[1].(*bool) = dirty
		return nil
	}}
}

func TestRunIsDefined(t *testing.T) {
	// Compile-level guard: main must keep a testable run() entry point.
	_ = run
}

func TestValidateModeRejectsDownWithWriteSnapshot(t *testing.T) {
	if err := validateMode(true, true); err == nil {
		t.Fatal("validateMode() must reject -down together with -write-snapshot")
	}
	if err := validateMode(false, true); err != nil {
		t.Fatalf("validateMode() rejected -write-snapshot: %v", err)
	}
	if err := validateMode(true, false); err != nil {
		t.Fatalf("validateMode() rejected -down: %v", err)
	}
}

func TestCurrentMigrationVersionMissingTable(t *testing.T) {
	q := &fakeMigrationQueryer{rows: []pgx.Row{boolRow(false)}}
	version, dirty, exists, err := currentMigrationVersion(context.Background(), q)
	if err != nil {
		t.Fatalf("currentMigrationVersion() error: %v", err)
	}
	if version != 0 || dirty || exists {
		t.Fatalf("got version=%d dirty=%v exists=%v, want 0 false false", version, dirty, exists)
	}
}

func TestCurrentMigrationVersionEmptyLedgerCanRetry(t *testing.T) {
	q := &fakeMigrationQueryer{rows: []pgx.Row{
		boolRow(true),
		fakeRow{scan: func(...any) error { return pgx.ErrNoRows }},
	}}
	version, dirty, exists, err := currentMigrationVersion(context.Background(), q)
	if err != nil {
		t.Fatalf("an empty schema_migrations should be retryable, got: %v", err)
	}
	if version != 0 || dirty || !exists {
		t.Fatalf("got version=%d dirty=%v exists=%v, want 0 false true", version, dirty, exists)
	}
}

func TestCurrentMigrationVersionReadsRecordedState(t *testing.T) {
	q := &fakeMigrationQueryer{rows: []pgx.Row{
		boolRow(true),
		versionRow(42, true),
	}}
	version, dirty, exists, err := currentMigrationVersion(context.Background(), q)
	if err != nil {
		t.Fatalf("currentMigrationVersion() error: %v", err)
	}
	if version != 42 || !dirty || !exists {
		t.Fatalf("got version=%d dirty=%v exists=%v, want 42 true true", version, dirty, exists)
	}
}

func TestCurrentMigrationVersionPropagatesOtherErrors(t *testing.T) {
	wantErr := errors.New("database unavailable")
	q := &fakeMigrationQueryer{rows: []pgx.Row{
		boolRow(true),
		fakeRow{scan: func(...any) error { return wantErr }},
	}}
	_, _, _, err := currentMigrationVersion(context.Background(), q)
	if !errors.Is(err, wantErr) {
		t.Fatalf("currentMigrationVersion() error = %v, want %v", err, wantErr)
	}
}
