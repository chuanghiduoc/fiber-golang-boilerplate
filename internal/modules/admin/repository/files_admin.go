package repository

import (
	"context"
	"io"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	adminsvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
)

// FilesAdmin maps the files contract onto the admin service port.
type FilesAdmin struct {
	contracts filescontracts.FilesAdmin
}

var _ adminsvc.FilesAdmin = (*FilesAdmin)(nil)

func NewFilesAdmin(contracts filescontracts.FilesAdmin) *FilesAdmin {
	return &FilesAdmin{contracts: contracts}
}

func (r *FilesAdmin) ListFiles(ctx context.Context, limit int, startingAfter string) ([]adminsvc.File, bool, error) {
	page, err := r.contracts.ListFiles(ctx, limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	files := make([]adminsvc.File, len(page.Items))
	for i := range page.Items {
		files[i] = toFile(page.Items[i])
	}
	return files, page.HasMore, nil
}

func (r *FilesAdmin) OpenFile(ctx context.Context, id int64) (*adminsvc.File, io.ReadCloser, error) {
	download, err := r.contracts.OpenFile(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	file := toFile(download.File)
	return &file, download.Reader, nil
}

func (r *FilesAdmin) DeleteFile(ctx context.Context, id int64) error {
	return r.contracts.DeleteFile(ctx, id)
}

func (r *FilesAdmin) RestoreFile(ctx context.Context, id int64) (*adminsvc.File, error) {
	file, err := r.contracts.RestoreFile(ctx, id)
	if err != nil {
		return nil, err
	}
	mapped := toFile(file)
	return &mapped, nil
}

func (r *FilesAdmin) GetFileStats(ctx context.Context) (adminsvc.FileStats, error) {
	stats, err := r.contracts.GetFileStats(ctx)
	if err != nil {
		return adminsvc.FileStats{}, err
	}
	return adminsvc.FileStats{
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}, nil
}

func toFile(file filescontracts.File) adminsvc.File {
	return adminsvc.File{
		ID:           file.ID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
}
