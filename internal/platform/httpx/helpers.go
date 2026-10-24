// Package httpx contains HTTP helpers shared by module handlers without
// pulling in the legacy handler package (which would create an import cycle).
package httpx

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
	var q dto.CursorQuery
	if err := c.Bind().Query(&q); err != nil {
		return 0, "", apperror.NewBadRequest("invalid query parameters")
	}
	return q.Limit, q.StartingAfter, nil
}
