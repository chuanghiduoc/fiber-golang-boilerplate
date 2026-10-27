package files

import (
	"time"

	"github.com/google/uuid"
)

// Domain events published by the files module. The envelope (id,
// type, dataVersion, occurredAt, tenantId, actor) is
// internal/platform/events.Event; these structs are its data. They are public
// webhook contracts, deliberately separate from the REST DTOs: evolve them
// additively, and bump the version constant (or add a new type) for any
// breaking change.
const (
	// EventFileUploaded is published after a file is stored.
	EventFileUploaded = "file.uploaded"
	// FileUploadedVersion is the current FileUploaded data version.
	FileUploadedVersion = 1

	// EventFileDeleted is published after a file is soft-deleted (by its
	// owner or an admin takedown).
	EventFileDeleted = "file.deleted"
	// FileDeletedVersion is the current FileDeleted data version.
	FileDeletedVersion = 1

	// EventFileRestored is published after a soft-deleted file is restored.
	EventFileRestored = "file.restored"
	// FileRestoredVersion is the current FileRestored data version.
	FileRestoredVersion = 1
)

// FileUploaded is the data of file.uploaded, version 1.
type FileUploaded struct {
	FileID       uuid.UUID `json:"fileId"`
	OwnerID      uuid.UUID `json:"ownerId"`
	OriginalName string    `json:"originalName"`
	MimeType     string    `json:"mimeType"`
	Size         int64     `json:"size"`
	CreatedAt    time.Time `json:"createdAt"`
}

// FileDeleted is the data of file.deleted, version 1.
type FileDeleted struct {
	FileID       uuid.UUID `json:"fileId"`
	OwnerID      uuid.UUID `json:"ownerId"`
	OriginalName string    `json:"originalName"`
	DeletedAt    time.Time `json:"deletedAt"`
}

// FileRestored is the data of file.restored, version 1.
type FileRestored struct {
	FileID       uuid.UUID `json:"fileId"`
	OwnerID      uuid.UUID `json:"ownerId"`
	OriginalName string    `json:"originalName"`
}
