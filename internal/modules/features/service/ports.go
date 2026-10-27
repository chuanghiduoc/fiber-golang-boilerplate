package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
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

// EvaluatedFlag is one flag's effective value for the request tenant.
type EvaluatedFlag struct {
	Key        string
	Enabled    bool
	KillSwitch bool
	Source     string
}

// Target is a tenant-wide (UserID nil) or user target.
type Target struct {
	ID        uuid.UUID
	FlagKey   string
	TenantID  uuid.UUID
	UserID    *uuid.UUID
	Enabled   bool
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// TargetRef addresses a target by its canonical item id. A nil TenantID means
// the request tenant; a nil UserID means the tenant-wide target.
type TargetRef struct {
	TenantID *uuid.UUID
	UserID   *uuid.UUID
}

// UpsertFlagInput is a flag write. A nil Enabled clears the database default
// so environment and compiled defaults apply again. A nil Description keeps
// the row's current description.
type UpsertFlagInput struct {
	Enabled     *bool
	KillSwitch  *bool
	Description *string
}

// ExpectedVersion is the If-Match precondition of a flag write. A zero value is
// unconditional; otherwise Exists says whether the caller expects a row and
// UpdatedAt is the validator it read. The repository enforces it in the same
// statement as the write, so a concurrent change cannot be lost.
type ExpectedVersion struct {
	Unconditional bool
	Exists        bool
	UpdatedAt     time.Time
}

// UnconditionalVersion writes without a precondition.
func UnconditionalVersion() ExpectedVersion { return ExpectedVersion{Unconditional: true} }

// FlagWrite is the resolved flag state the repository persists.
type FlagWrite struct {
	Enabled     *bool
	KillSwitch  bool
	Description string
}

// Repository is the persistence port. Implementations scope target queries to
// the requested tenant and map rows to service types. A target write for a
// tenant other than the context tenant is a platform cross-tenant operation
// and uses the row-level-security bypass for that statement.
type Repository interface {
	ListFlags(ctx context.Context) ([]Flag, error)
	GetFlag(ctx context.Context, key string) (Flag, error)
	CreateFlag(ctx context.Context, key string, write FlagWrite) (Flag, error)
	CreateFlagIfAbsent(ctx context.Context, key string, write FlagWrite) (Flag, error)
	UpsertFlag(ctx context.Context, key string, write FlagWrite) (Flag, error)
	UpdateFlagIfUnchanged(ctx context.Context, key string, write FlagWrite, expected time.Time) (Flag, error)
	DeleteFlag(ctx context.Context, key string) error
	DeleteFlagIfUnchanged(ctx context.Context, key string, expected time.Time) error
	ListTargets(ctx context.Context, key string) ([]Target, error)
	ListAllTargets(ctx context.Context, key string) ([]Target, error)
	UpsertTarget(ctx context.Context, key string, tenantID uuid.UUID, userID *uuid.UUID, enabled bool) (Target, error)
	DeleteTarget(ctx context.Context, key string, tenantID uuid.UUID, userID *uuid.UUID) error
}

// AuditWriter records a Tier 1 audit event in the current transaction.
type AuditWriter interface {
	RecordTx(ctx context.Context, event audit.Event) error
}

// TxRepos are the collaborators bound to one transaction.
type TxRepos struct {
	Flags Repository
	Audit AuditWriter
}

// Transactor runs fn in one transaction: a failed audit row rolls the flag
// mutation back with it.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}
