package core

import (
	"context"
	"errors"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// EmailTaken is the public 409 for a registration that reuses an address.
func EmailTaken() error {
	return apperror.NewCode(identitycatalog.CodeUserEmailTaken, "email already registered")
}

// EmailUnavailable conceals a retained ban marker for the address.
func EmailUnavailable() error {
	return apperror.NewConflict("email is not available")
}

// AccountNotActive is the 401 for a member whose account or membership is no
// longer active.
func AccountNotActive() error {
	return apperror.NewUnauthorized("account is not active")
}

// EnsureEmailNotBanned keeps an address banned in the request tenant from
// joining it again, even after the banned account deleted itself. The check
// uses the keyed marker (active and every previous key), never a plaintext
// address comparison or an unkeyed hash.
func EnsureEmailNotBanned(ctx context.Context, st Stores, email string) error {
	banned, err := st.EmailBanned(ctx, email)
	switch {
	case err != nil:
		return err
	case banned:
		return EmailUnavailable()
	default:
		return nil
	}
}

// CreateAccount creates a global user, its password (when hash is set), and a
// membership with role in the request tenant through st.
func CreateAccount(ctx context.Context, st Stores, user NewUser, hash, role string) (*Member, error) {
	created, err := st.Users.Create(ctx, user)
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			return nil, EmailTaken()
		}
		return nil, apperror.NewInternal("failed to create user")
	}
	if hash != "" {
		if err := st.Users.CreatePassword(ctx, created.ID, hash); err != nil {
			return nil, apperror.NewInternal("failed to create user")
		}
		created.HasPassword = true
	}
	membership, err := st.Memberships.Create(ctx, created.ID, role)
	if err != nil {
		return nil, apperror.NewInternal("failed to create membership")
	}
	return &Member{User: *created, Membership: *membership}, nil
}

// ValidatePasswordForEmail applies the password/email policy shared by
// registration and invitation sign-up.
func ValidatePasswordForEmail(password, email, field string) error {
	if !validator.PasswordContainsEmailLocalPart(password, email) {
		return nil
	}
	return apperror.NewValidation("One or more fields did not pass validation.", []apperror.FieldError{{
		Path:    field,
		Code:    validator.CodePasswordFromEmail,
		Message: field + " must not be or contain your email address",
	}})
}
