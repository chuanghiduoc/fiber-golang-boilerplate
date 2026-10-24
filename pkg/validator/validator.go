package validator

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-playground/validator/v10"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

var (
	once     sync.Once
	validate *validator.Validate
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

func validatePassword(fl validator.FieldLevel) bool {
	password := fl.Field().String()
	if utf8.RuneCountInString(password) < 8 || len(password) > 72 {
		return false
	}
	var hasUpper, hasLower, hasDigit, hasSpecial bool
	for _, ch := range password {
		switch {
		case unicode.IsUpper(ch):
			hasUpper = true
		case unicode.IsLower(ch):
			hasLower = true
		case unicode.IsDigit(ch):
			hasDigit = true
		case unicode.IsPunct(ch) || unicode.IsSymbol(ch):
			hasSpecial = true
		}
	}
	return hasUpper && hasLower && hasDigit && hasSpecial
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
			Code:    codeForTag(fe.Tag()),
			Message: formatError(fe, path),
			Params:  paramsForError(fe),
		})
	}

	return apperror.NewValidation("One or more fields did not pass validation.", fields)
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
		return "weak_password"
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
		return fmt.Sprintf("%s must be 8-72 characters with uppercase, lowercase, digit, and special character", path)
	case "notblank":
		return fmt.Sprintf("%s must not be blank", path)
	default:
		return fmt.Sprintf("%s is invalid", path)
	}
}
