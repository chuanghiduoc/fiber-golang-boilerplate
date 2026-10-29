package service

import (
	"bytes"
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
// one bounded JSON document, so without a bound a user with large files could
// exhaust the worker's memory. Files past the budget are listed with
// contentOmitted and stay downloadable through the files API.
const exportInlineLimit = privacycontracts.DefaultByteLimit

// PrivacyRepository is the files side of data export/erasure.
type PrivacyRepository interface {
	ListForUserLimited(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]File, bool, error)
	HardDelete(ctx context.Context, tenantID, userID, id uuid.UUID) error
}

// PrivacyExporter returns one bounded page of file metadata and, within
// exportInlineLimit, blob contents (base64) for one user in one tenant.
type PrivacyExporter struct {
	inlineLimit int64
	repo        PrivacyRepository
	storage     storage.Storage
}

func NewPrivacyExporter(repo PrivacyRepository, store storage.Storage) *PrivacyExporter {
	return &PrivacyExporter{inlineLimit: exportInlineLimit, repo: repo, storage: store}
}

func (e *PrivacyExporter) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	files, rowTruncated, err := e.repo.ListForUserLimited(ctx, tenantID, userID, privacycontracts.DefaultRowLimit)
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
		Files  []exportedFile          `json:"files"`
		Budget privacycontracts.Budget `json:"budget"`
	}{Files: make([]exportedFile, 0, len(files))}
	remaining := e.inlineLimit
	truncated := rowTruncated
	for _, file := range files {
		item := exportedFile{
			ID: file.ID, Name: file.OriginalName, MimeType: file.MimeType,
			Size: file.Size, CreatedAt: file.CreatedAt, DeletedAt: file.DeletedAt,
		}
		if remaining <= 0 {
			// Do not issue a storage request once the byte budget is
			// exhausted. Every later file is still represented by metadata.
			item.ContentOmitted = true
			truncated = true
			out.Files = append(out.Files, item)
			continue
		}
		if file.Size > remaining {
			// The stored size is only a hint: readBase64 still caps the bytes
			// it reads, so a corrupt/understated size cannot exceed the budget.
			// A known oversized blob is skipped before allocating its base64.
			item.ContentOmitted = true
			truncated = true
			out.Files = append(out.Files, item)
			continue
		}
		content, rawBytes, fits, err := e.readBase64(ctx, file.StoragePath, remaining)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			item.ContentMissing = true
		case err != nil:
			return privacycontracts.Section{}, fmt.Errorf("read file %s: %w", file.ID, err)
		case !fits:
			item.ContentOmitted = true
			truncated = true
		default:
			item.ContentBase64 = content
			remaining -= rawBytes
		}
		out.Files = append(out.Files, item)
	}
	out.Budget = privacycontracts.Budget{
		RowLimit:  privacycontracts.DefaultRowLimit,
		ByteLimit: e.inlineLimit,
		Returned:  len(out.Files),
		Truncated: truncated,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "files", Data: data, Truncated: truncated}, nil
}

// readBase64 streams the blob through a base64 encoder when it fits in limit
// bytes. It reads at most limit+1 bytes, so the stored size column is never
// trusted, and it avoids retaining both the raw blob and its encoded form.
func (e *PrivacyExporter) readBase64(ctx context.Context, path string, limit int64) (encoded string, rawBytes int64, fits bool, err error) {
	reader, err := e.storage.Get(ctx, path)
	if err != nil {
		return "", 0, false, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	var buf bytes.Buffer
	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	rawBytes, copyErr := io.Copy(encoder, io.LimitReader(reader, limit+1))
	closeErr := encoder.Close()
	if copyErr != nil {
		return "", 0, false, copyErr
	}
	if closeErr != nil {
		return "", 0, false, closeErr
	}
	if rawBytes > limit {
		return "", rawBytes, false, nil
	}
	return buf.String(), rawBytes, true, nil
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
	for {
		files, _, err := e.repo.ListForUserLimited(ctx, tenantID, userID, privacycontracts.DefaultRowLimit)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return nil
		}
		for _, file := range files {
			if err := e.storage.Delete(ctx, file.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("delete file blob %s: %w", file.ID, err)
			}
			if err := e.repo.HardDelete(ctx, tenantID, userID, file.ID); err != nil {
				return fmt.Errorf("delete file row %s: %w", file.ID, err)
			}
		}
	}
}
