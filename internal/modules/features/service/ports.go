package service

import (
	"context"
	"time"
)

// Flag is a feature flag's database state plus the value the service derives
// for the requesting subject. Enabled is the database default when Configured
// is true, otherwise the compiled default.
type Flag struct {
	Key         string
	Description string
	Enabled     bool
	KillSwitch  bool
	Configured  bool
	Effective   bool
	Source      string
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}

// FlagDetail is a flag and every target in the current tenant.
type FlagDetail struct {
	Flag    Flag
	Targets []Target
}

// Target is a tenant-wide (UserID nil) or user target.
type Target struct {
	ID        int64
	FlagKey   string
	TenantID  int64
	UserID    *int64
	Enabled   bool
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// UpsertFlagInput is a flag write. A nil Enabled clears the database default
// so environment and compiled defaults apply again.
type UpsertFlagInput struct {
	Enabled     *bool
	KillSwitch  *bool
	Description *string
}

// FlagWrite is the resolved flag state the repository persists.
type FlagWrite struct {
	Enabled     *bool
	KillSwitch  bool
	Description string
}

// UpsertTargetInput creates or replaces a tenant-wide (nil UserID) or user
// target in the request tenant.
type UpsertTargetInput struct {
	UserID  *int64
	Enabled *bool
}

// Repository is the persistence port. Implementations scope target queries to
// the request tenant and map rows to service types.
type Repository interface {
	ListFlags(ctx context.Context) ([]Flag, error)
	GetFlag(ctx context.Context, key string) (Flag, error)
	UpsertFlag(ctx context.Context, key string, write FlagWrite) (Flag, error)
	DeleteFlag(ctx context.Context, key string) error
	ListTargets(ctx context.Context, key string) ([]Target, error)
	UpsertTarget(ctx context.Context, key string, userID int64, enabled bool) (Target, error)
	DeleteTarget(ctx context.Context, key string, id int64) error
}
