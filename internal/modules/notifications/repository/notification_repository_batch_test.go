package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

type batchExecDB struct {
	rows  []int64
	calls int
}

func (d *batchExecDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	if len(args) != 3 {
		return pgconn.CommandTag{}, fmt.Errorf("exec args = %d, want 3", len(args))
	}
	if d.calls >= len(d.rows) {
		return pgconn.CommandTag{}, fmt.Errorf("unexpected extra batch")
	}
	rows := d.rows[d.calls]
	d.calls++
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", rows)), nil
}

func (*batchExecDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("unexpected query")
}

func (*batchExecDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return nil
}

func TestMarkAllReadRepeatsUntilShortBatch(t *testing.T) {
	db := &batchExecDB{rows: []int64{int64(markAllReadBatchSize), 250, 0}}
	repo := NewNotificationRepository(db)
	updated, err := repo.MarkAllRead(tenant.WithID(context.Background(), tenant.DefaultID), uuid.New())
	if err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	want := int64(markAllReadBatchSize) + 250
	if updated != want {
		t.Fatalf("updated = %d, want %d", updated, want)
	}
	if db.calls != 2 {
		t.Fatalf("batches = %d, want 2", db.calls)
	}
}

func TestMarkAllReadStopsAfterExactMultiple(t *testing.T) {
	db := &batchExecDB{rows: []int64{int64(markAllReadBatchSize), int64(markAllReadBatchSize), 0}}
	repo := NewNotificationRepository(db)
	updated, err := repo.MarkAllRead(tenant.WithID(context.Background(), tenant.DefaultID), uuid.New())
	if err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	if want := int64(markAllReadBatchSize) * 2; updated != want {
		t.Fatalf("updated = %d, want %d", updated, want)
	}
	if db.calls != 3 {
		t.Fatalf("batches = %d, want 3", db.calls)
	}
}
