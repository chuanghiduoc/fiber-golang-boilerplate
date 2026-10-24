package service

import (
	"context"
	"fmt"
	"io"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Service is the admin module's application surface. It composes the two
// narrow owner contracts and returns admin-owned DTOs; it never reaches into
// identity/files repositories, generated SQL, storage, sessions, or a
// database transaction.
type Service interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]admindto.UserResponse, bool, error)
	UpdateRole(ctx context.Context, id int64, role string) (*admindto.UserResponse, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*admindto.UserResponse, error)
	ListFiles(ctx context.Context, limit int, startingAfter string) ([]admindto.FileResponse, bool, error)
	Download(ctx context.Context, id int64) (*admindto.FileResponse, io.ReadCloser, error)
	DeleteFile(ctx context.Context, id int64) error
	RestoreFile(ctx context.Context, id int64) (*admindto.FileResponse, error)
	GetStats(ctx context.Context) (*admindto.AdminStatsResponse, error)
}

type service struct {
	identity identitycontracts.IdentityAdmin
	files    filescontracts.FilesAdmin
}

func NewService(identity identitycontracts.IdentityAdmin, files filescontracts.FilesAdmin) Service {
	return &service{identity: identity, files: files}
}

func (s *service) ListUsers(ctx context.Context, limit int, startingAfter string) ([]admindto.UserResponse, bool, error) {
	page, err := s.identity.ListUsers(ctx, limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	users := make([]admindto.UserResponse, len(page.Items))
	for i := range page.Items {
		users[i] = toUserResponse(page.Items[i])
	}
	return users, page.HasMore, nil
}

func (s *service) UpdateRole(ctx context.Context, id int64, role string) (*admindto.UserResponse, error) {
	if role != admindto.RoleUser && role != admindto.RoleAdmin {
		return nil, apperror.NewBadRequest("role must be one of: user, admin")
	}
	user, err := s.identity.UpdateRole(ctx, id, role)
	if err != nil {
		return nil, err
	}
	resp := toUserResponse(user)
	return &resp, nil
}

func (s *service) BanUser(ctx context.Context, id int64) error {
	return s.identity.BanUser(ctx, id)
}

func (s *service) UnbanUser(ctx context.Context, id int64) (*admindto.UserResponse, error) {
	user, err := s.identity.UnbanUser(ctx, id)
	if err != nil {
		return nil, err
	}
	resp := toUserResponse(user)
	return &resp, nil
}

func (s *service) ListFiles(ctx context.Context, limit int, startingAfter string) ([]admindto.FileResponse, bool, error) {
	page, err := s.files.ListFiles(ctx, limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	files := make([]admindto.FileResponse, len(page.Items))
	for i := range page.Items {
		files[i] = toFileResponse(page.Items[i])
	}
	return files, page.HasMore, nil
}

func (s *service) Download(ctx context.Context, id int64) (*admindto.FileResponse, io.ReadCloser, error) {
	download, err := s.files.OpenFile(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	resp := toFileResponse(download.File)
	return &resp, download.Reader, nil
}

func (s *service) DeleteFile(ctx context.Context, id int64) error {
	return s.files.DeleteFile(ctx, id)
}

func (s *service) RestoreFile(ctx context.Context, id int64) (*admindto.FileResponse, error) {
	file, err := s.files.RestoreFile(ctx, id)
	if err != nil {
		return nil, err
	}
	resp := toFileResponse(file)
	return &resp, nil
}

func (s *service) GetStats(ctx context.Context) (*admindto.AdminStatsResponse, error) {
	users, err := s.identity.GetUserStats(ctx)
	if err != nil {
		return nil, apperror.NewInternal("failed to get system stats")
	}
	files, err := s.files.GetFileStats(ctx)
	if err != nil {
		return nil, apperror.NewInternal("failed to get system stats")
	}
	return &admindto.AdminStatsResponse{
		ActiveUsers:   users.ActiveUsers,
		DeletedUsers:  users.DeletedUsers,
		TotalFiles:    files.TotalFiles,
		TotalFileSize: files.TotalFileSize,
	}, nil
}

func toUserResponse(user identitycontracts.User) admindto.UserResponse {
	return admindto.UserResponse{
		ID:            user.ID,
		Email:         user.Email,
		Name:          user.Name,
		Role:          user.Role,
		EmailVerified: user.EmailVerified,
		HasPassword:   user.HasPassword,
		CreatedAt:     user.CreatedAt,
		UpdatedAt:     user.UpdatedAt,
		DeletedAt:     user.DeletedAt,
		BannedAt:      user.BannedAt,
	}
}

func toFileResponse(file filescontracts.File) admindto.FileResponse {
	resp := admindto.FileResponse{
		ID:           file.ID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
	if file.DeletedAt == nil {
		resp.URL = fmt.Sprintf("/api/v1/admin/files/%d/download", file.ID)
	}
	return resp
}
