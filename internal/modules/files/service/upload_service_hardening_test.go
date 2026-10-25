package service

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
)

// PostgreSQL rejects invalid UTF-8 (22021), so a crafted filename must not
// reach the metadata insert after the blob was already written.
func TestCleanOriginalName_ProducesValidUTF8(t *testing.T) {
	for _, name := range []string{"bad\xffname.png", strings.Repeat("\xff", maxOriginalNameRunes+10)} {
		if got := cleanOriginalName(name); !utf8.ValidString(got) {
			t.Fatalf("cleanOriginalName(%q) = %q, not valid UTF-8", name, got)
		}
	}
}

func TestUploadList_IncludeDeleted(t *testing.T) {
	repo := newMockFileRepo()
	svc := newTestUploadService(repo, newMockStorage())
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	repo.files[1] = &filesqlc.File{ID: 1, TenantID: 1, UserID: 1, OriginalName: "active.txt", CreatedAt: now}
	repo.files[2] = &filesqlc.File{ID: 2, TenantID: 1, UserID: 1, OriginalName: "deleted.txt", CreatedAt: now, DeletedAt: now}
	repo.nextID = 3

	activeOnly, _, err := svc.List(systemCtx(), 1, 10, "", false)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(activeOnly) != 1 || activeOnly[0].ID != 1 {
		t.Fatalf("active list = %+v, want only file 1", activeOnly)
	}

	withDeleted, _, err := svc.List(systemCtx(), 1, 10, "", true)
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
