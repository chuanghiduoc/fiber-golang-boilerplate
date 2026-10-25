package service

import (
	"context"
	"errors"
	"time"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// File is the files module's view of one metadata row. DeletedAt is nil for a
// live file and set for a soft-deleted one.
type File struct {
	ID           int64
	TenantID     int64
	UserID       int64
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
	UserID       int64
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

// FileRepository is the metadata persistence port. Implementations scope every
// query to the request tenant (tenant.EffectiveID(ctx)) and return
// apperror.ErrNotFound when no row matches.
type FileRepository interface {
	Create(ctx context.Context, file NewFile) (*File, error)
	GetByID(ctx context.Context, id int64) (*File, error)
	GetByIDIncludingDeleted(ctx context.Context, id int64) (*File, error)
	ListByUserCursor(ctx context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]File, error)
	AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]File, error)
	Delete(ctx context.Context, id int64) (*File, error)
	Restore(ctx context.Context, id int64) (*File, error)
	AdminFileStats(ctx context.Context) (FileStats, error)
}

// AuditWriter records a Tier 1 audit event in the current transaction, so the
// event commits or rolls back with the mutation.
type AuditWriter interface {
	RecordTx(ctx context.Context, event audit.Event) error
}

// Outbox enqueues webhook events in the current transaction.
type Outbox interface {
	Enqueue(ctx context.Context, events ...webhookcontracts.Event) error
}

// TxRepos are the collaborators bound to one transaction.
type TxRepos struct {
	Files FileRepository
	Audit AuditWriter
	// Outbox is nil when the wired webhook dispatcher cannot join the
	// transaction; callers then dispatch after commit.
	Outbox Outbox
}

// Transactor runs fn in one transaction: when fn returns an error every write
// made through tx rolls back, otherwise all of them commit together.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error
}
