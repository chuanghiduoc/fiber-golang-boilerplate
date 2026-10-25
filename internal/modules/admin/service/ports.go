package service

import (
	"context"
	"io"
	"time"
)

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User is one member of the request tenant as the admin surface exposes it.
type User struct {
	ID               int64
	Email            string
	Name             string
	Role             string
	MembershipStatus string
	EmailVerified    bool
	HasPassword      bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	JoinedAt         time.Time
	DeletedAt        *time.Time
	BannedAt         *time.Time
}

// File is one stored file as the admin surface exposes it. DeletedAt is set
// for a soft-deleted file.
type File struct {
	ID           int64
	UserID       int64
	OriginalName string
	MimeType     string
	Size         int64
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

// UserStats is the identity-owned half of the admin dashboard.
type UserStats struct {
	ActiveUsers  int64
	DeletedUsers int64
}

// FileStats is the files-owned half of the admin dashboard.
type FileStats struct {
	TotalFiles    int64
	TotalFileSize int64
}

// Stats is the dashboard aggregation over both owner modules.
type Stats struct {
	ActiveUsers   int64
	DeletedUsers  int64
	TotalFiles    int64
	TotalFileSize int64
}

// IdentityAdmin is the admin module's port over the identity back office.
type IdentityAdmin interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]User, bool, error)
	UpdateRole(ctx context.Context, id int64, role string) (*User, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*User, error)
	GetUserStats(ctx context.Context) (UserStats, error)
}

// FilesAdmin is the admin module's port over the files back office.
type FilesAdmin interface {
	ListFiles(ctx context.Context, limit int, startingAfter string) ([]File, bool, error)
	OpenFile(ctx context.Context, id int64) (*File, io.ReadCloser, error)
	DeleteFile(ctx context.Context, id int64) error
	RestoreFile(ctx context.Context, id int64) (*File, error)
	GetFileStats(ctx context.Context) (FileStats, error)
}
