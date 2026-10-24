// Package contracts exposes narrow, transport-neutral interfaces that modules
// use to read or command another bounded context without importing its
// internals. Implementations live in the owning module's adapter package.
package contracts

import (
	"context"
	"time"
)

// User is the identity module's cross-module view of an account. It
// deliberately omits credentials, token versions, and other internal fields.
type User struct {
	ID            int64
	Email         string
	Name          string
	Role          string
	EmailVerified bool
	HasPassword   bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
	BannedAt      *time.Time
}

// UserPage is one cursor page of users plus the next-page indicator.
type UserPage struct {
	Items   []User
	HasMore bool
}

// UserStats is the identity-owned half of the admin dashboard.
type UserStats struct {
	ActiveUsers  int64
	DeletedUsers int64
}

// Principal is the identity-owned view needed to authenticate a request on
// behalf of an API key's owner. It intentionally excludes credentials and
// session fields.
type Principal struct {
	ID       int64
	TenantID int64
	Email    string
	Name     string
	Role     string
}

// PrincipalReader loads the current principal for a tenant/user pair. It is
// used by API-key authentication so bans, deletions, and role changes take
// effect immediately. Implementations must return apperror.ErrNotFound (or a
// wrapped error) when the account is not active.
type PrincipalReader interface {
	GetPrincipal(ctx context.Context, tenantID, userID int64) (Principal, error)
}

// IdentityAdmin is the back-office surface identity exposes to admin.
type IdentityAdmin interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) (UserPage, error)
	UpdateRole(ctx context.Context, id int64, role string) (User, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (User, error)
	GetUserStats(ctx context.Context) (UserStats, error)
}
