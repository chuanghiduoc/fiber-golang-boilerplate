package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// PrivacyRepository is the files side of data export/erasure.
type PrivacyRepository interface {
	ListForUser(ctx context.Context, tenantID, userID int64) ([]File, error)
	HardDelete(ctx context.Context, tenantID, userID, id int64) error
}

// PrivacyExporter returns file metadata and blob contents (base64) for one
// user in one tenant.
type PrivacyExporter struct {
	repo    PrivacyRepository
	storage storage.Storage
}

func NewPrivacyExporter(repo PrivacyRepository, store storage.Storage) *PrivacyExporter {
	return &PrivacyExporter{repo: repo, storage: store}
}

func (e *PrivacyExporter) Export(ctx context.Context, tenantID, userID int64) (privacycontracts.Section, error) {
	files, err := e.repo.ListForUser(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	type exportedFile struct {
		ID             int64      `json:"id"`
		Name           string     `json:"name"`
		MimeType       string     `json:"mimeType"`
		Size           int64      `json:"size"`
		CreatedAt      time.Time  `json:"createdAt"`
		DeletedAt      *time.Time `json:"deletedAt,omitempty"`
		ContentBase64  string     `json:"contentBase64,omitempty"`
		ContentMissing bool       `json:"contentMissing,omitempty"`
	}
	out := struct {
		Files []exportedFile `json:"files"`
	}{Files: make([]exportedFile, 0, len(files))}
	for _, file := range files {
		item := exportedFile{
			ID: file.ID, Name: file.OriginalName, MimeType: file.MimeType,
			Size: file.Size, CreatedAt: file.CreatedAt, DeletedAt: file.DeletedAt,
		}
		content, err := e.read(ctx, file.StoragePath)
		switch {
		case err == nil:
			item.ContentBase64 = base64.StdEncoding.EncodeToString(content)
		case errors.Is(err, storage.ErrNotFound):
			item.ContentMissing = true
		default:
			return privacycontracts.Section{}, fmt.Errorf("read file %d: %w", file.ID, err)
		}
		out.Files = append(out.Files, item)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "files", Data: data}, nil
}

func (e *PrivacyExporter) read(ctx context.Context, path string) ([]byte, error) {
	reader, err := e.storage.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

// PrivacyEraser deletes blobs, then rows. A missing blob counts as deleted.
type PrivacyEraser struct {
	repo    PrivacyRepository
	storage storage.Storage
}

func NewPrivacyEraser(repo PrivacyRepository, store storage.Storage) *PrivacyEraser {
	return &PrivacyEraser{repo: repo, storage: store}
}

func (e *PrivacyEraser) Erase(ctx context.Context, tenantID, userID int64) error {
	files, err := e.repo.ListForUser(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := e.storage.Delete(ctx, file.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete file blob %d: %w", file.ID, err)
		}
		if err := e.repo.HardDelete(ctx, tenantID, userID, file.ID); err != nil {
			return fmt.Errorf("delete file row %d: %w", file.ID, err)
		}
	}
	return nil
}
