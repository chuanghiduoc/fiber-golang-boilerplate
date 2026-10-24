package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

func seedAdminUsers(repo *mockUserRepo, n int) {
	for i := 0; i < n; i++ {
		repo.users[repo.nextID] = &identitysqlc.User{TenantID: 1,
			ID:        repo.nextID,
			Email:     "user@example.com",
			Name:      "User",
			Role:      dto.RoleUser,
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		repo.nextID++
	}
}

func newAdminUserServiceForTest() (*adminUserService, *mockUserRepo, *mockRefreshTokenRepo) {
	ur := newMockUserRepo()
	rtr := newMockRefreshTokenRepo()
	svc := NewAdminUserService(ur, rtr, nil).(*adminUserService)
	return svc, ur, rtr
}

func TestAdminListUsers(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, _ := newAdminUserServiceForTest()
		seedAdminUsers(ur, 3)

		users, hasMore, err := svc.ListUsers(context.Background(), 10, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hasMore {
			t.Errorf("expected hasMore=false for a full page")
		}
		if len(users) != 3 {
			t.Errorf("len(users) = %d, want 3", len(users))
		}
	})

	t.Run("empty list", func(t *testing.T) {
		svc, _, _ := newAdminUserServiceForTest()

		users, hasMore, err := svc.ListUsers(context.Background(), 10, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hasMore {
			t.Errorf("expected hasMore=false for an empty list")
		}
		if len(users) != 0 {
			t.Errorf("len(users) = %d, want 0", len(users))
		}
	})

	t.Run("invalid cursor", func(t *testing.T) {
		svc, _, _ := newAdminUserServiceForTest()

		_, _, err := svc.ListUsers(context.Background(), 10, "not-a-cursor")
		if err == nil {
			t.Fatal("expected invalid cursor error")
		}
	})
}

func TestAdminUpdateRole(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, _ := newAdminUserServiceForTest()
		seedAdminUsers(ur, 1)

		user, err := svc.UpdateRole(context.Background(), 1, dto.RoleAdmin)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user.Role != dto.RoleAdmin {
			t.Errorf("role = %q, want admin", user.Role)
		}
	})

	t.Run("invalid role", func(t *testing.T) {
		svc, _, _ := newAdminUserServiceForTest()

		_, err := svc.UpdateRole(context.Background(), 1, "superuser")
		if err == nil {
			t.Fatal("expected invalid role error")
		}
	})

	t.Run("user not found", func(t *testing.T) {
		svc, _, _ := newAdminUserServiceForTest()

		_, err := svc.UpdateRole(context.Background(), 999, dto.RoleAdmin)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestAdminBanUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, rtr := newAdminUserServiceForTest()
		seedAdminUsers(ur, 1)

		// Create a refresh token for the user.
		rtr.tokens["hash1"] = &identitysqlc.RefreshToken{TenantID: 1, UserID: 1, Token: "hash1"}

		err := svc.BanUser(context.Background(), 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// User should be soft-deleted, not removed from the table.
		if u, ok := ur.users[1]; !ok || !u.DeletedAt.Valid || !u.BannedAt.Valid {
			t.Error("user should be soft-deleted and marked banned")
		}

		// Refresh tokens should be revoked.
		if len(rtr.deletedUserIDs) == 0 || rtr.deletedUserIDs[0] != 1 {
			t.Error("refresh tokens should be revoked for banned user")
		}
	})

	t.Run("user not found", func(t *testing.T) {
		svc, _, _ := newAdminUserServiceForTest()

		err := svc.BanUser(context.Background(), 999)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestAdminUnbanUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, rtr := newAdminUserServiceForTest()
		seedAdminUsers(ur, 1)
		ur.users[1].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		rtr.tokens["hash1"] = &identitysqlc.RefreshToken{TenantID: 1, UserID: 1, Token: "hash1"}

		user, err := svc.UnbanUser(context.Background(), 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user == nil {
			t.Fatal("user should not be nil")
		}
		if ur.users[1].DeletedAt.Valid || ur.users[1].BannedAt.Valid {
			t.Error("user should be restored and unbanned")
		}
		if len(rtr.deletedUserIDs) == 0 || rtr.deletedUserIDs[0] != 1 {
			t.Error("refresh tokens should be cleared for unbanned user")
		}
	})

	t.Run("active user is not banned", func(t *testing.T) {
		svc, ur, _ := newAdminUserServiceForTest()
		seedAdminUsers(ur, 1)

		if _, err := svc.UnbanUser(context.Background(), 1); err == nil {
			t.Fatal("expected not-found for an active user")
		}
	})

	t.Run("user not found", func(t *testing.T) {
		svc, _, _ := newAdminUserServiceForTest()

		_, err := svc.UnbanUser(context.Background(), 999)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestAdmin_CannotRemoveLastAdmin(t *testing.T) {
	svc, repo, _ := newAdminUserServiceForTest()
	repo.users[1] = &identitysqlc.User{TenantID: 1,
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

func TestAdminGetUserStats(t *testing.T) {
	svc, ur, _ := newAdminUserServiceForTest()
	seedAdminUsers(ur, 5)
	ur.users[2].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}

	stats, err := svc.GetUserStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.ActiveUsers != 4 {
		t.Errorf("ActiveUsers = %d, want 4", stats.ActiveUsers)
	}
	if stats.DeletedUsers != 1 {
		t.Errorf("DeletedUsers = %d, want 1", stats.DeletedUsers)
	}
}
