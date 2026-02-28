package validator

import (
	"fmt"
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

	details := make(map[string]string, len(validationErrors))
	for _, fe := range validationErrors {
		details[fe.Field()] = formatError(fe)
	}

	return apperror.NewValidation("validation failed", details)
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
