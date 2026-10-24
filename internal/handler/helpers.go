package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// paramID extracts and validates a required int64 path parameter.
func paramID(c fiber.Ctx, name string) (int64, error) {
	return httpx.ParamID(c, name)
}

// authUserID returns the authenticated user's ID from the JWT context.
// Key is set by middleware.JWTAuth.
func authUserID(c fiber.Ctx) int64 {
	return httpx.AuthUserID(c)
}

// authRole returns the authenticated user's role from the JWT context.
func authRole(c fiber.Ctx) string {
	return fiber.Locals[string](c, "role")
}

// bindAndValidate parses the request body and runs struct validation. Unknown
// fields and trailing JSON values are rejected so client typos cannot silently
// pass validation.
func bindAndValidate(c fiber.Ctx, req any) error {
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

// contentDispositionAttachment builds an RFC 6266 Content-Disposition header for
// a downloadable file. It is implemented in platform/httpx so module handlers
// and legacy handlers share the same security-sensitive logic.
func contentDispositionAttachment(name string) string {
	return httpx.ContentDispositionAttachment(name)
}

// cursorQuery binds the limit + startingAfter cursor pagination params.
func cursorQuery(c fiber.Ctx) (limit int, startingAfter string, err error) {
	return httpx.CursorQuery(c)
}

// nextUserCursor/nextFileCursor expose the opaque cursor clients must pass as
// startingAfter on the next request. Without this, hasMore=true is not
// actionable.
func nextUserCursor(items []dto.UserResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}

func nextFileCursor(items []dto.FileResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}
