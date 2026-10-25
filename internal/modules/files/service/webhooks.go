package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
)

// fileUploadedData is the file.uploaded payload. It mirrors the JSON of the
// user-facing dto.FileResponse field for field (names, order, omitempty) so
// the payload bytes did not change when the service stopped returning DTOs.
type fileUploadedData struct {
	ID           int64      `json:"id"`
	TenantID     int64      `json:"tenantId"`
	UserID       int64      `json:"userId,omitempty"`
	OriginalName string     `json:"originalName"`
	MimeType     string     `json:"mimeType"`
	Size         int64      `json:"size"`
	URL          string     `json:"url"`
	CreatedAt    time.Time  `json:"createdAt"`
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}

func uploadedEventData(file *File) fileUploadedData {
	data := fileUploadedData{
		ID:           file.ID,
		TenantID:     file.TenantID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
	if file.DeletedAt == nil {
		data.URL = fmt.Sprintf("/api/v1/files/%d/download", file.ID)
	}
	return data
}

// fileEventData is the payload of file.deleted and file.restored events.
func fileEventData(file *File) map[string]any {
	return map[string]any{
		"id":           file.ID,
		"tenantId":     file.TenantID,
		"userId":       file.UserID,
		"originalName": file.OriginalName,
	}
}

// enqueueWebhook enqueues one domain event through the transaction's outbox.
func enqueueWebhook(ctx context.Context, outbox Outbox, eventType string, tenantID int64, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	return outbox.Enqueue(ctx, webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	})
}

// dispatchWebhookAfterCommit is the post-commit fallback used when no
// transactional outbox is wired. Failures are logged only, because the
// mutation has already committed.
func dispatchWebhookAfterCommit(ctx context.Context, dispatcher webhookcontracts.Dispatcher, eventType string, tenantID int64, data any) {
	if dispatcher == nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Warn("failed to marshal webhook payload", slog.String("event", eventType), slog.Any("error", err))
		return
	}
	if _, err := dispatcher.Dispatch(context.WithoutCancel(ctx), webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	}); err != nil {
		slog.Warn("failed to dispatch webhook event", slog.String("event", eventType), slog.Any("error", err))
	}
}
