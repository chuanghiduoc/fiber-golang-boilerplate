package handler

import (
	"context"
	"log/slog"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
	"github.com/google/uuid"
)

// AccountUsers is the profile and credential surface the user handlers use.
// *authn.UserService implements it.
type AccountUsers interface {
	GetByID(ctx context.Context, id uuid.UUID) (*core.Member, error)
	Update(ctx context.Context, id uuid.UUID, req authn.UpdateProfileInput) (*core.Member, error)
	VerifyPassword(ctx context.Context, userID uuid.UUID, password string) error
	ChangePassword(ctx context.Context, userID uuid.UUID, req authn.ChangePasswordInput) error
}

// AccountVerification is the verification surface the user handlers use.
// *authn.EmailVerificationService implements it.
type AccountVerification interface {
	Invalidate(ctx context.Context, userID uuid.UUID) error
	SendVerification(ctx context.Context, userID uuid.UUID, userEmail string) error
}

type UserHandler struct {
	service       AccountUsers
	emailVerifSvc AccountVerification
}

func NewUserHandler(svc AccountUsers, emailVerif ...AccountVerification) *UserHandler {
	h := &UserHandler{service: svc}
	if len(emailVerif) > 0 {
		h.emailVerifSvc = emailVerif[0]
	}
	return h
}

type profileInput struct {
	Authorization string `header:"Authorization"`
}

type updateProfileInput struct {
	RawBody []byte `rawBody:"application/merge-patch+json" contentType:"application/merge-patch+json" required:"true"`
}

type changePasswordInput struct {
	Body dto.ChangePasswordRequest
}

// GetMe returns the caller's profile in the request tenant.
func (h *UserHandler) GetMe(ctx context.Context, _ *profileInput) (*httpapi.Output[dto.UserResponse], error) {
	member, err := h.service.GetByID(ctx, actorID(ctx))
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.UserResponse]{Body: toUserResponse(member)}, nil
}

// UpdateMe applies an RFC 7396 merge patch to the caller's global profile.
func (h *UserHandler) UpdateMe(ctx context.Context, input *updateProfileInput) (*httpapi.Output[dto.UserResponse], error) {
	var req dto.ProfilePatch
	if err := decodeMergePatch(input.RawBody, &req); err != nil {
		return nil, err
	}
	if req.Name.Present && req.Name.Null {
		return nil, unprocessable("name cannot be cleared")
	}
	if req.Email.Present && req.Email.Null {
		return nil, unprocessable("email cannot be cleared")
	}
	if !req.Name.Present && !req.Email.Present {
		member, err := h.service.GetByID(ctx, actorID(ctx))
		if err != nil {
			return nil, err
		}
		return &httpapi.Output[dto.UserResponse]{Body: toUserResponse(member)}, nil
	}

	var name, email *string
	if req.Name.Present {
		value := req.Name.Value
		name = &value
	}
	if req.Email.Present {
		value := req.Email.Value
		email = &value
	}

	emailChanged, err := h.requirePasswordForEmailChange(ctx, actorID(ctx), email, req.CurrentPassword)
	if err != nil {
		return nil, err
	}

	member, err := h.service.Update(ctx, actorID(ctx), authn.UpdateProfileInput{Name: name, Email: email})
	if err != nil {
		return nil, err
	}
	h.sendVerificationAfterEmailChange(ctx, member, emailChanged)

	return &httpapi.Output[dto.UserResponse]{Body: toUserResponse(member)}, nil
}

// requirePasswordForEmailChange forces re-authentication before a user can
// move their account to a different address.
func (h *UserHandler) requirePasswordForEmailChange(ctx context.Context, userID uuid.UUID, email *string, password patch.Field[string]) (bool, error) {
	if email == nil {
		return false, nil
	}
	current, err := h.service.GetByID(ctx, userID)
	if err != nil {
		return false, err
	}
	if core.NormalizeEmail(*email) == core.NormalizeEmail(current.User.Email) {
		return false, nil
	}
	if !current.User.HasPassword {
		return false, apperror.NewBadRequest("email changes are not available for passwordless accounts")
	}
	if !password.Present || password.Value == "" {
		return false, apperror.NewBadRequest("currentPassword is required to change email")
	}
	if err := h.service.VerifyPassword(withClientMetadata(ctx), userID, password.Value); err != nil {
		return false, err
	}
	return true, nil
}

// sendVerificationAfterEmailChange invalidates tokens sent to the old address
// and sends a fresh verification link to the new one.
func (h *UserHandler) sendVerificationAfterEmailChange(ctx context.Context, member *core.Member, emailChanged bool) {
	if !emailChanged || member.User.EmailVerified() || h.emailVerifSvc == nil {
		return
	}
	if err := h.emailVerifSvc.Invalidate(ctx, member.User.ID); err != nil {
		return
	}
	// SendVerification enqueues the email job in its own transaction; the
	// profile change has already committed, so a failure is logged and does
	// not turn a successful update into an error.
	if err := h.emailVerifSvc.SendVerification(ctx, member.User.ID, member.User.Email); err != nil {
		slog.Error("failed to enqueue verification email", slog.String("user_id", member.User.ID.String()), slog.Any("error", err))
	}
}

// ChangePassword changes the caller's password.
func (h *UserHandler) ChangePassword(ctx context.Context, input *changePasswordInput) (*httpapi.Empty, error) {
	member, err := h.service.GetByID(ctx, actorID(ctx))
	if err != nil {
		return nil, err
	}
	if err := core.ValidatePasswordForEmail(input.Body.NewPassword, member.User.Email, "newPassword"); err != nil {
		return nil, err
	}
	if err := h.service.ChangePassword(withClientMetadata(ctx), actorID(ctx), authn.ChangePasswordInput{
		CurrentPassword: input.Body.CurrentPassword,
		NewPassword:     input.Body.NewPassword,
	}); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}
