package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
)

func TestUploadList_IncludeDeleted(t *testing.T) {
	repo := newMockFileRepo()
	svc := newTestUploadService(repo, newMockStorage())
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	repo.files[1] = &filesqlc.File{ID: 1, TenantID: 1, UserID: 1, OriginalName: "active.txt", CreatedAt: now}
	repo.files[2] = &filesqlc.File{ID: 2, TenantID: 1, UserID: 1, OriginalName: "deleted.txt", CreatedAt: now, DeletedAt: now}
	repo.nextID = 3

	activeOnly, _, err := svc.List(context.Background(), 1, 10, "", false)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(activeOnly) != 1 || activeOnly[0].ID != 1 {
		t.Fatalf("active list = %+v, want only file 1", activeOnly)
	}

	withDeleted, _, err := svc.List(context.Background(), 1, 10, "", true)
	if err != nil {
		t.Fatalf("list with deleted: %v", err)
	}
	if len(withDeleted) != 2 {
		t.Fatalf("includeDeleted list = %d items, want 2", len(withDeleted))
	}
	for _, f := range withDeleted {
		if f.ID == 2 && f.DeletedAt == nil {
			t.Error("deleted file must expose deletedAt")
		}
	}
}
