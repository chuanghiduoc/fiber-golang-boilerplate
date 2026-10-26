package service

import (
	"context"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// System role keys. "user" is accepted as the legacy name of member.
const (
	RoleMember     = "member"
	RoleAdmin      = "admin"
	RoleOwner      = "owner"
	roleLegacyUser = "user"
)

// Stats is the dashboard aggregation over both owner modules.
type Stats struct {
	ActiveUsers   int64
	DeletedUsers  int64
	TotalFiles    int64
	TotalFileSize int64
}

// Service is the admin module's application surface. It composes the two
// narrow owner contracts and returns their stdlib-only types; it never reaches
// into identity or files repositories, generated SQL, storage, sessions, or a
// database transaction.
type Service interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) (identitycontracts.UserPage, error)
	UpdateRole(ctx context.Context, id int64, role string) (identitycontracts.User, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (identitycontracts.User, error)
	ListFiles(ctx context.Context, limit int, startingAfter string) (filescontracts.FilePage, error)
	Download(ctx context.Context, id int64) (filescontracts.FileDownload, error)
	DeleteFile(ctx context.Context, id int64) error
	RestoreFile(ctx context.Context, id int64) (filescontracts.File, error)
	GetStats(ctx context.Context) (Stats, error)
}

type service struct {
	identity identitycontracts.IdentityAdmin
	files    filescontracts.FilesAdmin
}

func NewService(identity identitycontracts.IdentityAdmin, files filescontracts.FilesAdmin) Service {
	return &service{identity: identity, files: files}
}

func (s *service) ListUsers(ctx context.Context, limit int, startingAfter string) (identitycontracts.UserPage, error) {
	return s.identity.ListUsers(ctx, limit, startingAfter)
}

func (s *service) UpdateRole(ctx context.Context, id int64, role string) (identitycontracts.User, error) {
	if role == roleLegacyUser {
		role = RoleMember
	}
	if role != RoleMember && role != RoleAdmin && role != RoleOwner {
		return identitycontracts.User{}, apperror.NewBadRequest("role must be one of: member, admin, owner")
	}
	return s.identity.UpdateRole(ctx, id, role)
}

func (s *service) BanUser(ctx context.Context, id int64) error {
	return s.identity.BanUser(ctx, id)
}

func (s *service) UnbanUser(ctx context.Context, id int64) (identitycontracts.User, error) {
	return s.identity.UnbanUser(ctx, id)
}

func (s *service) ListFiles(ctx context.Context, limit int, startingAfter string) (filescontracts.FilePage, error) {
	return s.files.ListFiles(ctx, limit, startingAfter)
}

func (s *service) Download(ctx context.Context, id int64) (filescontracts.FileDownload, error) {
	return s.files.OpenFile(ctx, id)
}

func (s *service) DeleteFile(ctx context.Context, id int64) error {
	return s.files.DeleteFile(ctx, id)
}

func (s *service) RestoreFile(ctx context.Context, id int64) (filescontracts.File, error) {
	return s.files.RestoreFile(ctx, id)
}

func (s *service) GetStats(ctx context.Context) (Stats, error) {
	users, err := s.identity.GetUserStats(ctx)
	if err != nil {
		return Stats{}, apperror.NewInternal("failed to get system stats")
	}
	files, err := s.files.GetFileStats(ctx)
	if err != nil {
		return Stats{}, apperror.NewInternal("failed to get system stats")
	}
	return Stats{
		ActiveUsers:   users.ActiveUsers,
		DeletedUsers:  users.DeletedUsers,
		TotalFiles:    files.TotalFiles,
		TotalFileSize: files.TotalFileSize,
	}, nil
}
