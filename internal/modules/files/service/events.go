package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
)

func fileUploaded(file *File) filescontracts.FileUploaded {
	return filescontracts.FileUploaded{
		FileID:       file.ID,
		OwnerID:      file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
	}
}

// fileDeleted builds the file.deleted data from the row the soft delete
// returned, so deletedAt is the committed timestamp.
func fileDeleted(file *File) filescontracts.FileDeleted {
	data := filescontracts.FileDeleted{
		FileID:       file.ID,
		OwnerID:      file.UserID,
		OriginalName: file.OriginalName,
	}
	if file.DeletedAt != nil {
		data.DeletedAt = *file.DeletedAt
	}
	return data
}

func fileRestored(file *File) filescontracts.FileRestored {
	return filescontracts.FileRestored{
		FileID:       file.ID,
		OwnerID:      file.UserID,
		OriginalName: file.OriginalName,
	}
}

// publishEvent publishes one domain event through the transaction, so the
// event (and every durable subscriber job, such as webhook fan-out) commits
// or rolls back with the mutation. The actor and request id come from ctx.
func publishEvent(ctx context.Context, publisher EventPublisher, eventType string, dataVersion int, tenantID uuid.UUID, subjectUserIDs []uuid.UUID, data any) error {
	event, err := events.NewEvent(eventType, dataVersion, data)
	if err != nil {
		return fmt.Errorf("build %s event: %w", eventType, err)
	}
	event.TenantID = tenantID
	if len(subjectUserIDs) > 0 {
		event.SubjectUserIDs = append([]uuid.UUID(nil), subjectUserIDs...)
	}
	if err := publisher.Publish(ctx, event); err != nil {
		slog.Error("failed to publish file event",
			slog.String("event", eventType),
			slog.Any("error", err),
		)
		return fmt.Errorf("publish %s event: %w", eventType, err)
	}
	return nil
}
