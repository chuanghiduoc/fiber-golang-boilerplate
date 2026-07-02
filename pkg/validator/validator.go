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
		fields = append(fields, apperror.FieldError{
			Path:    fe.Field(),
			Code:    codeForTag(fe.Tag()),
			Message: formatError(fe),
		})
	}

	return apperror.NewValidation("One or more fields did not pass validation.", fields)
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
	default:
		return "invalid"
	}
}

func formatError(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", fe.Field())
	case "email":
		return fmt.Sprintf("%s must be a valid email", fe.Field())
	case "min":
		return fmt.Sprintf("%s must be at least %s characters", fe.Field(), fe.Param())
	case "max":
		return fmt.Sprintf("%s must be at most %s characters", fe.Field(), fe.Param())
	case "password":
		return fmt.Sprintf("%s must be 8-72 characters with uppercase, lowercase, digit, and special character", fe.Field())
	default:
		return fmt.Sprintf("%s is invalid", fe.Field())
	}
}
