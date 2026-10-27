package validator

import (
	_ "embed"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-playground/validator/v10"
	"golang.org/x/text/unicode/norm"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Password policy bounds (NIST SP 800-63B): length in runes, never
// composition rules. Argon2id has no bcrypt-style input truncation, so there
// is no separate byte ceiling.
const (
	MinPasswordRunes = 12
	MaxPasswordRunes = 64
	// minEmailLocalPartRunes ignores a one- or two-character local part: it
	// would match almost any password and make the check unusable.
	minEmailLocalPartRunes = 3
)

// Validation field codes for the password policy.
const (
	CodeWeakPassword      = "weak_password"
	CodePasswordCommon    = "password_common"
	CodePasswordRepeated  = "password_repeated"
	CodePasswordBreached  = "password_breached"
	CodePasswordFromEmail = "password_contains_email"
)

// maxLeetVariants bounds the leetspeak expansions of one password, so a
// pathological input cannot multiply the blocklist lookups without limit. The
// canonical expansion (1 -> i) is always first, so it is never dropped.
const maxLeetVariants = 32

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

// NormalizePassword returns the canonical Unicode form used for every password
// operation: NFKC folds compatibility variants (ligatures, full-width forms,
// combined marks) to one representation, so a password set and offered with
// two visually identical spellings compares equal. It is applied to both the
// stored input (Argon2id) and the offered one. There is no deployed database,
// so this intentionally changes the hashing input; any hash created before
// this change would have to be re-set.
func NormalizePassword(password string) string {
	return norm.NFKC.String(password)
}

// PasswordWeakness classifies why password fails the policy and returns ""
// when it passes. The returned value is the stable validation error code. The
// NFKC form is measured and matched, and the optional breached-password check
// runs last so a local failure short-circuits the network call.
func PasswordWeakness(password string) string {
	normalized := NormalizePassword(password)
	switch {
	case utf8.RuneCountInString(normalized) < MinPasswordRunes:
		return CodeWeakPassword
	case utf8.RuneCountInString(normalized) > MaxPasswordRunes:
		return CodeWeakPassword
	case isRepetitivePassword(normalized):
		return CodePasswordRepeated
	case isCommonPassword(normalized):
		return CodePasswordCommon
	case isBreached(normalized):
		return CodePasswordBreached
	default:
		return ""
	}
}

func validatePassword(fl validator.FieldLevel) bool {
	return PasswordWeakness(fl.Field().String()) == ""
}

// isRepetitivePassword rejects a single repeated character and a short pattern
// repeated to fill the length ("abcabcabcabc"). Such values meet a length rule
// but carry almost no entropy.
func isRepetitivePassword(password string) bool {
	runes := []rune(strings.ToLower(password))
	if len(runes) < 2 {
		return false
	}
	for period := 1; period <= len(runes)/2; period++ {
		if len(runes)%period != 0 {
			continue
		}
		repeated := true
		for i := period; i < len(runes); i++ {
			if runes[i] != runes[i%period] {
				repeated = false
				break
			}
		}
		if repeated {
			return true
		}
	}
	return false
}

// isCommonPassword reports whether the password, or its "core", appears in the
// bundled top-10k list. The core is the lower-cased value with leading and
// trailing digits/punctuation removed and common leetspeak folded, so
// "P@ssw0rd2026" is recognized as "password" even though the full value is
// long enough to satisfy the length rule. A long passphrase that merely
// contains a common word is still accepted: only the full value and the
// digit/punctuation-trimmed core are matched.
func isCommonPassword(password string) bool {
	set := commonPasswordSet()
	lower := strings.ToLower(password)
	if _, found := set[lower]; found {
		return true
	}
	for _, variant := range leetVariants(lower) {
		if _, found := set[variant]; found {
			return true
		}
		if core := passwordCore(variant); core != "" {
			if _, found := set[core]; found {
				return true
			}
		}
	}
	return false
}

// leetReplacements maps the common leetspeak characters to their letters. '1'
// is ambiguous between i and l, so both spellings are produced.
var leetReplacements = map[rune][]rune{
	'0': {'o'},
	'1': {'i', 'l'},
	'3': {'e'},
	'4': {'a'},
	'5': {'s'},
	'7': {'t'},
	'@': {'a'},
	'$': {'s'},
}

// leetVariants expands the leetspeak characters of s (already lower-cased) into
// every plausible plain spelling, bounded by maxLeetVariants. The first
// element is the unchanged value.
func leetVariants(s string) []string {
	variants := []string{""}
	for _, r := range s {
		options := []rune{r}
		if replacements, ok := leetReplacements[r]; ok {
			options = append(options, replacements...)
		}
		next := make([]string, 0, len(variants)*len(options))
		for _, prefix := range variants {
			for _, option := range options {
				next = append(next, prefix+string(option))
			}
		}
		if len(next) > maxLeetVariants {
			next = next[:maxLeetVariants]
		}
		variants = next
	}
	return variants
}

// passwordCore trims leading and trailing digits and punctuation, leaving the
// letters that a padded common password is built around.
func passwordCore(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
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
		return fmt.Sprintf("%s must be 12-64 characters and must not be a commonly used password", path)
	case "notblank":
		return fmt.Sprintf("%s must not be blank", path)
	default:
		return fmt.Sprintf("%s is invalid", path)
	}
}
