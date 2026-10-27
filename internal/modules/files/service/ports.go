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
	// LockQuota serializes uploads of the context tenant for the rest of the
	// transaction. It must be called inside the upload's metadata transaction,
	// before StorageUsage, so two concurrent uploads cannot both observe the
	// same free space and both pass.
	LockQuota(ctx context.Context) error
	// StorageUsage returns the tenant's total stored bytes (live plus trash)
	// and the same total for one user.
	StorageUsage(ctx context.Context, userID uuid.UUID) (tenantBytes, userBytes int64, err error)
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
