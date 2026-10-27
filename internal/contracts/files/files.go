// Package files holds the stdlib-only contracts the files module provides to
// other modules (the admin back-office surface). Implementations live in
// internal/modules/files/adapter.
package files

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"
)

// File is the files module's cross-module view of stored metadata. Storage
// paths stay private to the owning module.
type File struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	UserID       uuid.UUID
	OriginalName string
	MimeType     string
	Size         int64
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

// FilePage is one cursor page of files plus the next-page indicator.
type FilePage struct {
	Items   []File
	HasMore bool
}

// FileStats is the files-owned half of the admin dashboard.
type FileStats struct {
	TotalFiles    int64
	TotalFileSize int64
}

// FileDownload pairs file metadata with an open storage reader. Callers own
// closing Reader.
type FileDownload struct {
	File   File
	Reader io.ReadCloser
}

// FilesAdmin is the back-office surface files exposes to admin.
type FilesAdmin interface {
	ListFiles(ctx context.Context, limit int, startingAfter string) (FilePage, error)
	OpenFile(ctx context.Context, id uuid.UUID) (FileDownload, error)
	DeleteFile(ctx context.Context, id uuid.UUID) error
	RestoreFile(ctx context.Context, id uuid.UUID) (File, error)
	GetFileStats(ctx context.Context) (FileStats, error)
}
