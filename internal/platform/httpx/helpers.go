// Package httpx contains HTTP helpers shared by module handlers without
// creating an import cycle.
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// ParamID extracts and validates a required int64 path parameter.
func ParamID(c fiber.Ctx, name string) (int64, error) {
	id := fiber.Params[int64](c, name)
	if id == 0 {
		return 0, apperror.NewBadRequest("invalid " + name)
	}
	return id, nil
}

// AuthUserID returns the authenticated user's ID from the JWT context.
// The key is set by middleware.JWTAuth.
func AuthUserID(c fiber.Ctx) int64 {
	return fiber.Locals[int64](c, "user_id")
}

// Can reports whether the authenticated subject holds permission. Use it
// instead of comparing the "role" local: the subject honors API-key scopes, so
// a key an admin issued with only profile scopes cannot act on other users.
func Can(c fiber.Ctx, permission authz.Permission) bool {
	subject, ok := authzhttp.SubjectFromFiber(c)
	return ok && subject.Has(permission)
}

// BindAndValidate parses the request body and runs struct validation. Unknown
// fields and trailing JSON values are rejected so client typos cannot silently
// pass validation.
func BindAndValidate(c fiber.Ctx, req any) error {
	body := c.Body()
	if len(body) == 0 {
		return apperror.NewBadRequest("request body is required")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return apperror.NewBadRequest("invalid JSON body")
		}
		return apperror.NewBadRequest("failed to parse request body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return apperror.NewBadRequest("invalid JSON body")
	}
	return validator.ValidateStruct(req)
}

// ContentDispositionAttachment builds an RFC 6266 Content-Disposition header
// for a downloadable file. It emits an ASCII-safe filename="" plus an RFC 5987
// filename*= for Unicode, and strips control/path characters from the
// client-supplied name to prevent header injection and path confusion.
func ContentDispositionAttachment(name string) string {
	// Use only the base name and drop any directory components.
	name = name[strings.LastIndexAny(name, "/\\")+1:]

	var ascii strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f: // control chars (incl. CR/LF)
			continue
		case r == '"' || r == '\\':
			ascii.WriteByte('_')
		case r > 0x7f: // non-ASCII → placeholder in the fallback token
			ascii.WriteByte('_')
		default:
			ascii.WriteRune(r)
		}
	}
	fallback := ascii.String()
	if fallback == "" {
		fallback = "download"
	}

	// filename* carries the exact UTF-8 name for clients that support RFC 5987.
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, url.PathEscape(name))
}

// CursorQuery binds the limit + startingAfter cursor pagination params.
func CursorQuery(c fiber.Ctx) (limit int, startingAfter string, err error) {
	var q struct {
		Limit         int    `query:"limit"`
		StartingAfter string `query:"startingAfter"`
	}
	if err := c.Bind().Query(&q); err != nil {
		return 0, "", apperror.NewBadRequest("invalid query parameters")
	}
	return q.Limit, q.StartingAfter, nil
}
