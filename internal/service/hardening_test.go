package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

func TestAdmin_CannotRemoveLastAdmin(t *testing.T) {
	svc, repo, _, _, _ := newAdminServiceForTest()
	repo.users[1] = &sqlc.User{
		ID: 1, Email: "admin@example.com", Name: "Admin", Role: dto.RoleAdmin,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	if _, err := svc.UpdateRole(context.Background(), 1, dto.RoleUser); err == nil {
		t.Fatal("expected last-admin demotion to be rejected")
	}
	if err := svc.BanUser(context.Background(), 1); err == nil {
		t.Fatal("expected last-admin ban to be rejected")
	}
}

func TestUploadList_IncludeDeleted(t *testing.T) {
	repo := newMockFileRepo()
	svc := newTestUploadService(repo, newMockStorage())
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	repo.files[1] = &sqlc.File{ID: 1, UserID: 1, OriginalName: "active.txt", CreatedAt: now}
	repo.files[2] = &sqlc.File{ID: 2, UserID: 1, OriginalName: "deleted.txt", CreatedAt: now, DeletedAt: now}
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
