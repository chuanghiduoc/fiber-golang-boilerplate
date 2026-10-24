// Package adapter exposes the files module's implementation of the
// cross-module admin contracts. It translates the module's DTOs into
// transport-neutral contract types so no caller imports files internals.
package adapter

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
)

// AdminFiles adapts the files admin service to contracts.FilesAdmin.
type AdminFiles struct {
	service service.AdminFileService
}

var _ contracts.FilesAdmin = (*AdminFiles)(nil)

func NewAdminFiles(svc service.AdminFileService) *AdminFiles {
	return &AdminFiles{service: svc}
}

func (a *AdminFiles) ListFiles(ctx context.Context, limit int, startingAfter string) (contracts.FilePage, error) {
	files, hasMore, err := a.service.ListFiles(ctx, limit, startingAfter)
	if err != nil {
		return contracts.FilePage{}, err
	}
	items := make([]contracts.File, len(files))
	for i := range files {
		items[i] = toFile(files[i])
	}
	return contracts.FilePage{Items: items, HasMore: hasMore}, nil
}

func (a *AdminFiles) OpenFile(ctx context.Context, id int64) (contracts.FileDownload, error) {
	file, reader, err := a.service.Download(ctx, id)
	if err != nil {
		return contracts.FileDownload{}, err
	}
	return contracts.FileDownload{File: toFile(*file), Reader: reader}, nil
}

func (a *AdminFiles) DeleteFile(ctx context.Context, id int64) error {
	return a.service.DeleteFile(ctx, id)
}

func (a *AdminFiles) RestoreFile(ctx context.Context, id int64) (contracts.File, error) {
	file, err := a.service.RestoreFile(ctx, id)
	if err != nil {
		return contracts.File{}, err
	}
	return toFile(*file), nil
}

func (a *AdminFiles) GetFileStats(ctx context.Context) (contracts.FileStats, error) {
	stats, err := a.service.GetFileStats(ctx)
	if err != nil {
		return contracts.FileStats{}, err
	}
	return contracts.FileStats{
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}, nil
}

func toFile(file dto.FileResponse) contracts.File {
	return contracts.File{
		ID:           file.ID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
}
