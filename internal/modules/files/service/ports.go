package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage/scan"
)

// File is the files module's view of one metadata row. DeletedAt is nil for a
// live file and set for a soft-deleted one.
type File struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	UserID       uuid.UUID
	OriginalName string
	StoragePath  string
	MimeType     string
	Size         int64
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

// NewFile is the input for creating a metadata row. The owning tenant is not a
// field: the repository always takes it from the request context, so a caller
// cannot create a file on behalf of another tenant.
type NewFile struct {
	UserID       uuid.UUID
	OriginalName string
	StoragePath  string
	MimeType     string
	Size         int64
}

// FileStats is the files-owned aggregation consumed by the admin module.
type FileStats struct {
	TotalFiles    int64
	TotalFileSize int64
}

// ErrStoragePathTaken is returned by FileRepository.Create when the storage
// path already exists (unique violation).
var ErrStoragePathTaken = errors.New("file storage path already exists")

// UploadOptions are the upload-time limits files enforces. Zero quotas are
// unlimited; a nil Scanner disables scanning.
type UploadOptions struct {
	TenantQuotaBytes int64
	UserQuotaBytes   int64
	Scanner          scan.Scanner
}

// QuotaScope selects the counters a quota check needs. User-only deployments
// must never read or lock tenant-wide usage.
type QuotaScope struct {
	Tenant bool
	User   bool
}

// QuotaUsage is the selected storage counters. UserBytes is one indexed row;
// TenantBytes is the SUM of users' rows only when the tenant quota is enabled.
// The unrequested side stays zero.
type QuotaUsage struct {
	TenantBytes int64
	UserBytes   int64
}

// FileRepository is the metadata persistence port. Implementations scope every
// query to the request tenant (tenant.Require(ctx)) and return
// apperror.ErrNotFound when no row matches.
type FileRepository interface {
	Create(ctx context.Context, file NewFile) (*File, error)
	GetByID(ctx context.Context, id uuid.UUID) (*File, error)
	GetByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*File, error)
	ListByUserCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor, includeDeleted bool) ([]File, error)
	AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]File, error)
	// ListCursor is the tenant-wide page for an admin caller of the collection
	// route; includeDeleted honours the caller's query parameter.
	ListCursor(ctx context.Context, cur pagination.DBCursor, includeDeleted bool) ([]File, error)
	Delete(ctx context.Context, id uuid.UUID) (*File, error)
	Restore(ctx context.Context, id uuid.UUID) (*File, error)
	AdminFileStats(ctx context.Context) (FileStats, error)
	// StorageUsage reads only the counters selected by scope. It is the cheap,
	// lock-free pre-check; user-only quota checks do not touch tenant usage.
	StorageUsage(ctx context.Context, userID uuid.UUID, scope QuotaScope) (QuotaUsage, error)
	// LockStorageUsage serializes only the selected quota scope and returns its
	// current values. It must run inside the upload's metadata transaction. A
	// user quota locks one user row; a tenant quota takes the tenant advisory
	// lock and sums per-user rows. The tenant lock is taken first when both are
	// enabled, so unrelated users stay concurrent under user-only quota.
	LockStorageUsage(ctx context.Context, userID uuid.UUID, scope QuotaScope) (QuotaUsage, error)
}

// AuditWriter records a Tier 1 audit event in the current transaction, so the
// event commits or rolls back with the mutation.
type AuditWriter interface {
	RecordTx(ctx context.Context, event audit.Event) error
}

// EventPublisher publishes a domain event in the current transaction
// (events.Bus.PublishTx): the event's durable subscriber jobs commit or roll
// back with the mutation.
type EventPublisher interface {
	Publish(ctx context.Context, event events.Event) error
}

// TxRepos are the collaborators bound to one transaction. Every field is
// non-nil.
type TxRepos struct {
	Files  FileRepository
	Audit  AuditWriter
	Events EventPublisher
}

// Transactor runs fn in one transaction: when fn returns an error every write
// made through tx rolls back, otherwise all of them commit together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}
