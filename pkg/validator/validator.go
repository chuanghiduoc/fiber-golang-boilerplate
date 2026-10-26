package validator

import (
	_ "embed"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/go-playground/validator/v10"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Password policy bounds (NIST SP 800-63B): length in runes, never
// composition rules. The byte ceiling is bcrypt's 72-byte input limit; a
// longer password would be silently truncated by bcrypt.
const (
	MinPasswordRunes = 12
	MaxPasswordRunes = 64
	MaxPasswordBytes = 72
	// minEmailLocalPartRunes ignores a one- or two-character local part: it
	// would match almost any password and make the check unusable.
	minEmailLocalPartRunes = 3
)

// Validation field codes for the password policy.
const (
	CodeWeakPassword      = "weak_password"
	CodePasswordCommon    = "password_common"
	CodePasswordFromEmail = "password_contains_email"
)

// commonPasswordsSource is the bundled list of the 10,000 most common
// passwords from OWASP SecLists (Passwords/Common-Credentials/10k-most-common.txt,
// MIT License, https://github.com/danielmiessler/SecLists). Matching is
// case-insensitive and exact.
//
//go:embed data/common_passwords.txt
var commonPasswordsSource string

var (
	once         sync.Once
	passwordOnce sync.Once
	validate     *validator.Validate

	commonPasswords map[string]struct{}
)

func instance() *validator.Validate {
	once.Do(func() {
		validate = validator.New()
		_ = validate.RegisterValidation("password", validatePassword)
		_ = validate.RegisterValidation("notblank", func(fl validator.FieldLevel) bool {
			return strings.TrimSpace(fl.Field().String()) != ""
		})
		// Report the JSON field name (camelCase) rather than the Go struct field
		// name, so error paths match the request/response contract.
		validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
			name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
			if name == "-" {
				return ""
			}
			return name
		})
	})
	return validate
}

// PasswordWeakness classifies why password fails the policy and returns ""
// when it passes. The returned value is the stable validation error code.
func PasswordWeakness(password string) string {
	switch {
	case utf8.RuneCountInString(password) < MinPasswordRunes:
		return CodeWeakPassword
	case utf8.RuneCountInString(password) > MaxPasswordRunes:
		return CodeWeakPassword
	case len(password) > MaxPasswordBytes:
		return CodeWeakPassword
	case isCommonPassword(password):
		return CodePasswordCommon
	default:
		return ""
	}
}

func validatePassword(fl validator.FieldLevel) bool {
	return PasswordWeakness(fl.Field().String()) == ""
}

// isCommonPassword reports whether the whole password appears in the bundled
// top-10k list. A password that merely contains a common word is not rejected:
// length is the dominant NIST control, and a long passphrase may legitimately
// contain one.
func isCommonPassword(password string) bool {
	set := commonPasswordSet()
	_, found := set[strings.ToLower(password)]
	return found
}

func commonPasswordSet() map[string]struct{} {
	passwordOnce.Do(func() {
		commonPasswords = make(map[string]struct{}, 10000)
		for _, line := range strings.Split(commonPasswordsSource, "\n") {
			password := strings.ToLower(strings.TrimSpace(line))
			if password != "" {
				commonPasswords[password] = struct{}{}
			}
		}
	})
	return commonPasswords
}

// PasswordContainsEmailLocalPart reports whether the password is or contains
// the local part of email (case-insensitive). It is a service-level check
// because the tag validator has no access to the address; registration,
// password change, and password reset call it where the email is known.
func PasswordContainsEmailLocalPart(password, email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	local := strings.ToLower(strings.TrimSpace(email[:at]))
	if utf8.RuneCountInString(local) < minEmailLocalPartRunes {
		return false
	}
	return strings.Contains(strings.ToLower(password), local)
}

func ValidateStruct(s interface{}) error {
	err := instance().Struct(s)
	if err == nil {
		return nil
	}

	validationErrors, ok := err.(validator.ValidationErrors)
	if !ok {
		return apperror.NewBadRequest("invalid request")
	}

	fields := make([]apperror.FieldError, 0, len(validationErrors))
	for _, fe := range validationErrors {
		path := fieldPath(fe)
		fields = append(fields, apperror.FieldError{
			Path:    path,
			Code:    fieldCode(fe),
			Message: formatError(fe, path),
			Params:  paramsForError(fe),
		})
	}

	return apperror.NewValidation("One or more fields did not pass validation.", fields)
}

// fieldCode maps a failing field to its stable snake_case error code (i18n
// key). The password tag is classified so a common-password rejection is
// distinguishable from a length violation.
func fieldCode(fe validator.FieldError) string {
	if fe.Tag() == "password" {
		if value, ok := fe.Value().(string); ok {
			if weakness := PasswordWeakness(value); weakness != "" {
				return weakness
			}
		}
	}
	return codeForTag(fe.Tag())
}

// fieldPath returns the JSON path of the failing field, such as
// "items[0].name". Namespace() is built from JSON tag names (see
// RegisterTagNameFunc) but starts with the Go root struct name, which is not
// part of the request contract, so that first segment is dropped. Field()
// alone would report only "name" and lose which element failed.
func fieldPath(fe validator.FieldError) string {
	namespace := fe.Namespace()
	if _, path, ok := strings.Cut(namespace, "."); ok && path != "" {
		return path
	}
	return fe.Field()
}

// codeForTag maps a validator tag to a stable snake_case error code (i18n key).
func codeForTag(tag string) string {
	switch tag {
	case "required":
		return "required"
	case "email":
		return "invalid_email"
	case "min":
		return "too_short"
	case "max":
		return "too_long"
	case "password":
		return CodeWeakPassword
	case "oneof":
		return "invalid_value"
	case "notblank":
		return "required"
	default:
		return "invalid"
	}
}

// paramsForError exposes interpolation values for localized validation
// messages. Only bounds carry parameters today; other codes use {path} only.
func paramsForError(fe validator.FieldError) map[string]string {
	switch fe.Tag() {
	case "min":
		return map[string]string{"min": fe.Param()}
	case "max":
		return map[string]string{"max": fe.Param()}
	default:
		return nil
	}
}

func formatError(fe validator.FieldError, path string) string {
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", path)
	case "email":
		return fmt.Sprintf("%s must be a valid email", path)
	case "min":
		return fmt.Sprintf("%s must be at least %s characters", path, fe.Param())
	case "max":
		return fmt.Sprintf("%s must be at most %s characters", path, fe.Param())
	case "password":
		return fmt.Sprintf("%s must be 12-64 characters (at most 72 bytes, the bcrypt limit) and must not be a commonly used password", path)
	case "notblank":
		return fmt.Sprintf("%s must not be blank", path)
	default:
		return fmt.Sprintf("%s is invalid", path)
	}
}
