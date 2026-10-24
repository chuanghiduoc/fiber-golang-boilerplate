package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// HTTP helpers shared with the legacy handler package live in platform/httpx.
// These unexported wrappers keep the module's call sites stable.

func paramID(c fiber.Ctx, name string) (int64, error) {
	return httpx.ParamID(c, name)
}

func authUserID(c fiber.Ctx) int64 {
	return httpx.AuthUserID(c)
}

func contentDispositionAttachment(name string) string {
	return httpx.ContentDispositionAttachment(name)
}

func cursorQuery(c fiber.Ctx) (limit int, startingAfter string, err error) {
	return httpx.CursorQuery(c)
}

// nextFileCursor exposes the opaque cursor clients must pass as startingAfter
// on the next request.
func nextFileCursor(items []dto.FileResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}
