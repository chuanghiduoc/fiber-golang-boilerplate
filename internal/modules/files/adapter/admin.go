// Package adapter exposes the files module's implementation of the
// cross-module admin contracts. It translates the module's DTOs into
// transport-neutral contract types so no caller imports files internals.
package adapter

import (
	"context"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
)

// AdminFiles adapts the files admin service to filescontracts.FilesAdmin.
type AdminFiles struct {
	service service.AdminFileService
}

var _ filescontracts.FilesAdmin = (*AdminFiles)(nil)

func NewAdminFiles(svc service.AdminFileService) *AdminFiles {
	return &AdminFiles{service: svc}
}

func (a *AdminFiles) ListFiles(ctx context.Context, limit int, startingAfter string) (filescontracts.FilePage, error) {
	files, hasMore, err := a.service.ListFiles(ctx, limit, startingAfter)
	if err != nil {
		return filescontracts.FilePage{}, err
	}
	items := make([]filescontracts.File, len(files))
	for i := range files {
		items[i] = toFile(files[i])
	}
	return filescontracts.FilePage{Items: items, HasMore: hasMore}, nil
}

func (a *AdminFiles) OpenFile(ctx context.Context, id int64) (filescontracts.FileDownload, error) {
	file, reader, err := a.service.Download(ctx, id)
	if err != nil {
		return filescontracts.FileDownload{}, err
	}
	return filescontracts.FileDownload{File: toFile(*file), Reader: reader}, nil
}

func (a *AdminFiles) DeleteFile(ctx context.Context, id int64) error {
	return a.service.DeleteFile(ctx, id)
}

func (a *AdminFiles) RestoreFile(ctx context.Context, id int64) (filescontracts.File, error) {
	file, err := a.service.RestoreFile(ctx, id)
	if err != nil {
		return filescontracts.File{}, err
	}
	return toFile(*file), nil
}

func (a *AdminFiles) GetFileStats(ctx context.Context) (filescontracts.FileStats, error) {
	stats, err := a.service.GetFileStats(ctx)
	if err != nil {
		return filescontracts.FileStats{}, err
	}
	return filescontracts.FileStats{
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}, nil
}

func toFile(file dto.FileResponse) filescontracts.File {
	return filescontracts.File{
		ID:           file.ID,
		TenantID:     file.TenantID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
}
