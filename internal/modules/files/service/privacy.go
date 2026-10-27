package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// exportInlineLimit bounds the blob bytes one export embeds. The export is
// one in-memory JSON document, so without a bound a user with large files
// could exhaust the worker's memory. Files past the budget are listed with
// contentOmitted and stay downloadable through the files API.
const exportInlineLimit = 64 << 20

// PrivacyRepository is the files side of data export/erasure.
type PrivacyRepository interface {
	ListForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]File, error)
	HardDelete(ctx context.Context, tenantID, userID, id uuid.UUID) error
}

// PrivacyExporter returns file metadata and, within exportInlineLimit, blob
// contents (base64) for one user in one tenant.
type PrivacyExporter struct {
	inlineLimit int64
	repo        PrivacyRepository
	storage     storage.Storage
}

func NewPrivacyExporter(repo PrivacyRepository, store storage.Storage) *PrivacyExporter {
	return &PrivacyExporter{inlineLimit: exportInlineLimit, repo: repo, storage: store}
}

func (e *PrivacyExporter) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	files, err := e.repo.ListForUser(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	type exportedFile struct {
		ID             uuid.UUID  `json:"id"`
		Name           string     `json:"name"`
		MimeType       string     `json:"mimeType"`
		Size           int64      `json:"size"`
		CreatedAt      time.Time  `json:"createdAt"`
		DeletedAt      *time.Time `json:"deletedAt,omitempty"`
		ContentBase64  string     `json:"contentBase64,omitempty"`
		ContentMissing bool       `json:"contentMissing,omitempty"`
		ContentOmitted bool       `json:"contentOmitted,omitempty"`
	}
	out := struct {
		Files []exportedFile `json:"files"`
	}{Files: make([]exportedFile, 0, len(files))}
	remaining := e.inlineLimit
	for _, file := range files {
		item := exportedFile{
			ID: file.ID, Name: file.OriginalName, MimeType: file.MimeType,
			Size: file.Size, CreatedAt: file.CreatedAt, DeletedAt: file.DeletedAt,
		}
		content, fits, err := e.read(ctx, file.StoragePath, remaining)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			item.ContentMissing = true
		case err != nil:
			return privacycontracts.Section{}, fmt.Errorf("read file %s: %w", file.ID, err)
		case !fits:
			item.ContentOmitted = true
		default:
			item.ContentBase64 = base64.StdEncoding.EncodeToString(content)
			remaining -= int64(len(content))
		}
		out.Files = append(out.Files, item)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "files", Data: data}, nil
}

// read returns the blob when it fits in limit bytes. It reads at most
// limit+1 bytes, so the stored size column is never trusted.
func (e *PrivacyExporter) read(ctx context.Context, path string, limit int64) (content []byte, fits bool, err error) {
	reader, err := e.storage.Get(ctx, path)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	content, err = io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > limit {
		return nil, false, nil
	}
	return content, true, nil
}

// PrivacyEraser deletes blobs, then rows. A missing blob counts as deleted.
type PrivacyEraser struct {
	repo    PrivacyRepository
	storage storage.Storage
}

func NewPrivacyEraser(repo PrivacyRepository, store storage.Storage) *PrivacyEraser {
	return &PrivacyEraser{repo: repo, storage: store}
}

func (e *PrivacyEraser) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	files, err := e.repo.ListForUser(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := e.storage.Delete(ctx, file.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete file blob %s: %w", file.ID, err)
		}
		if err := e.repo.HardDelete(ctx, tenantID, userID, file.ID); err != nil {
			return fmt.Errorf("delete file row %s: %w", file.ID, err)
		}
	}
	return nil
}
