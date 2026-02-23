package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

func seedUsers(repo *mockUserRepo, n int) {
	for i := 0; i < n; i++ {
		repo.users[repo.nextID] = &sqlc.User{
			ID:        repo.nextID,
			Email:     "user@example.com",
			Name:      "User",
			Role:      "user",
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		repo.nextID++
	}
}

func seedFiles(repo *mockFileRepo, n int) {
	for i := 0; i < n; i++ {
		repo.files[repo.nextID] = &sqlc.File{
			ID:           repo.nextID,
			UserID:       1,
			OriginalName: "file.txt",
			StoragePath:  "uploads/file.txt",
			MimeType:     "text/plain",
			Size:         1024,
			CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		repo.nextID++
	}
}

func newAdminServiceForTest() (*adminService, *mockUserRepo, *mockFileRepo, *mockRefreshTokenRepo, *mockStorage) {
	ur := newMockUserRepo()
	fr := newMockFileRepo()
	rtr := newMockRefreshTokenRepo()
	st := newMockStorage()
	svc := NewAdminService(ur, fr, rtr, st).(*adminService)
	return svc, ur, fr, rtr, st
}

func TestAdminListUsers(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, _, _, _ := newAdminServiceForTest()
		seedUsers(ur, 3)

		users, total, err := svc.ListUsers(context.Background(), 1, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		if len(users) != 3 {
			t.Errorf("len(users) = %d, want 3", len(users))
		}
	})

	t.Run("empty list", func(t *testing.T) {
		svc, _, _, _, _ := newAdminServiceForTest()

		users, total, err := svc.ListUsers(context.Background(), 1, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
		if len(users) != 0 {
			t.Errorf("len(users) = %d, want 0", len(users))
		}
	})
}

func TestAdminUpdateRole(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, _, _, _ := newAdminServiceForTest()
		seedUsers(ur, 1)

		user, err := svc.UpdateRole(context.Background(), 1, "admin")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user.Role != "admin" {
			t.Errorf("role = %q, want admin", user.Role)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		svc, _, _, _, _ := newAdminServiceForTest()

		_, err := svc.UpdateRole(context.Background(), 999, "admin")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestAdminBanUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, _, rtr, _ := newAdminServiceForTest()
		seedUsers(ur, 1)

		// Create a refresh token for the user
		rtr.tokens["hash1"] = &sqlc.RefreshToken{UserID: 1, Token: "hash1"}

		err := svc.BanUser(context.Background(), 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// User should be deleted
		if _, ok := ur.users[1]; ok {
			t.Error("user should be deleted from repo")
		}

		// Refresh tokens should be revoked
		if len(rtr.deletedUserIDs) == 0 || rtr.deletedUserIDs[0] != 1 {
			t.Error("refresh tokens should be revoked for banned user")
		}
	})

	t.Run("user not found", func(t *testing.T) {
		svc, _, _, _, _ := newAdminServiceForTest()

		err := svc.BanUser(context.Background(), 999)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestAdminUnbanUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, _, _, _ := newAdminServiceForTest()
		seedUsers(ur, 1)

		user, err := svc.UnbanUser(context.Background(), 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user == nil {
			t.Fatal("user should not be nil")
		}
	})

	t.Run("user not found", func(t *testing.T) {
		svc, _, _, _, _ := newAdminServiceForTest()

		_, err := svc.UnbanUser(context.Background(), 999)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestAdminListFiles(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, _, fr, _, _ := newAdminServiceForTest()
		seedFiles(fr, 2)

		files, total, err := svc.ListFiles(context.Background(), 1, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
		if len(files) != 2 {
			t.Errorf("len(files) = %d, want 2", len(files))
		}
		// Check that URL is constructed via storage
		for _, f := range files {
			if f.URL == "" {
				t.Error("file URL should not be empty")
			}
		}
	})

	t.Run("empty list", func(t *testing.T) {
		svc, _, _, _, _ := newAdminServiceForTest()

		files, total, err := svc.ListFiles(context.Background(), 1, 10)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
		if len(files) != 0 {
			t.Errorf("len(files) = %d, want 0", len(files))
		}
	})
}

func TestAdminGetStats(t *testing.T) {
	svc, ur, _, _, _ := newAdminServiceForTest()
	seedUsers(ur, 5)

	stats, err := svc.GetStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.ActiveUsers != 5 {
		t.Errorf("ActiveUsers = %d, want 5", stats.ActiveUsers)
	}
}
