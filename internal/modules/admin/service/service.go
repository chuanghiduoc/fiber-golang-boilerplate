package service

import (
	"context"
	"io"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Service is the admin module's application surface. It composes the two
// owner ports and returns admin-owned types; it never reaches into identity or
// files repositories, generated SQL, storage, sessions, or a database
// transaction.
type Service interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]User, bool, error)
	UpdateRole(ctx context.Context, id int64, role string) (*User, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*User, error)
	ListFiles(ctx context.Context, limit int, startingAfter string) ([]File, bool, error)
	Download(ctx context.Context, id int64) (*File, io.ReadCloser, error)
	DeleteFile(ctx context.Context, id int64) error
	RestoreFile(ctx context.Context, id int64) (*File, error)
	GetStats(ctx context.Context) (*Stats, error)
}

type service struct {
	identity IdentityAdmin
	files    FilesAdmin
}

func NewService(identity IdentityAdmin, files FilesAdmin) Service {
	return &service{identity: identity, files: files}
}

func (s *service) ListUsers(ctx context.Context, limit int, startingAfter string) ([]User, bool, error) {
	return s.identity.ListUsers(ctx, limit, startingAfter)
}

func (s *service) UpdateRole(ctx context.Context, id int64, role string) (*User, error) {
	if role != RoleUser && role != RoleAdmin {
		return nil, apperror.NewBadRequest("role must be one of: user, admin")
	}
	return s.identity.UpdateRole(ctx, id, role)
}

func (s *service) BanUser(ctx context.Context, id int64) error {
	return s.identity.BanUser(ctx, id)
}

func (s *service) UnbanUser(ctx context.Context, id int64) (*User, error) {
	return s.identity.UnbanUser(ctx, id)
}

func (s *service) ListFiles(ctx context.Context, limit int, startingAfter string) ([]File, bool, error) {
	return s.files.ListFiles(ctx, limit, startingAfter)
}

func (s *service) Download(ctx context.Context, id int64) (*File, io.ReadCloser, error) {
	return s.files.OpenFile(ctx, id)
}

func (s *service) DeleteFile(ctx context.Context, id int64) error {
	return s.files.DeleteFile(ctx, id)
}

func (s *service) RestoreFile(ctx context.Context, id int64) (*File, error) {
	return s.files.RestoreFile(ctx, id)
}

func (s *service) GetStats(ctx context.Context) (*Stats, error) {
	users, err := s.identity.GetUserStats(ctx)
	if err != nil {
		return nil, apperror.NewInternal("failed to get system stats")
	}
	files, err := s.files.GetFileStats(ctx)
	if err != nil {
		return nil, apperror.NewInternal("failed to get system stats")
	}
	return &Stats{
		ActiveUsers:   users.ActiveUsers,
		DeletedUsers:  users.DeletedUsers,
		TotalFiles:    files.TotalFiles,
		TotalFileSize: files.TotalFileSize,
	}, nil
}
