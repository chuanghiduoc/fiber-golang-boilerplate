package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// paramID extracts and validates a required int64 path parameter.
func paramID(c fiber.Ctx, name string) (int64, error) {
	id := fiber.Params[int64](c, name)
	if id == 0 {
		return 0, apperror.NewBadRequest("invalid " + name)
	}
	return id, nil
}

// authUserID returns the authenticated user's ID from the JWT context.
// Key is set by middleware.JWTAuth.
func authUserID(c fiber.Ctx) int64 {
	return fiber.Locals[int64](c, "user_id")
}

// authRole returns the authenticated user's role from the JWT context.
func authRole(c fiber.Ctx) string {
	return fiber.Locals[string](c, "role")
}

// bindAndValidate parses the request body and runs struct validation.
func bindAndValidate(c fiber.Ctx, req any) error {
	if err := c.Bind().Body(req); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return apperror.NewBadRequest("invalid JSON body")
		}
		return apperror.NewBadRequest("failed to parse request body")
	}
	return validator.ValidateStruct(req)
}

// contentDispositionAttachment builds an RFC 6266 Content-Disposition header for
// a downloadable file. It emits an ASCII-safe filename="" plus an RFC 5987
// filename*= for Unicode, and strips control/path characters from the
// client-supplied name to prevent header injection and path confusion.
func contentDispositionAttachment(name string) string {
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

// cursorQuery binds the limit + startingAfter cursor pagination params.
func cursorQuery(c fiber.Ctx) (limit int, startingAfter string, err error) {
	var q dto.CursorQuery
	if err := c.Bind().Query(&q); err != nil {
		return 0, "", apperror.NewBadRequest("invalid query parameters")
	}
	return q.Limit, q.StartingAfter, nil
}
