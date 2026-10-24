package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// BodyLimit rejects non-multipart request bodies larger than limit with a 413
// problem response. Fiber's global BodyLimit stays at the upload ceiling
// (APP_BODY_LIMIT) because multipart uploads need it; this middleware applies
// the much smaller APP_JSON_BODY_LIMIT to every other content type, before
// routing reaches auth, rate limiting, and JSON decoding.
//
// Residual risk: Fiber/fasthttp buffers the whole body before middleware runs,
// so a non-multipart request (including chunked bodies without
// Content-Length) can still make the server read up to APP_BODY_LIMIT before
// it is rejected here. Keep a reverse-proxy limit in front of the API.
func BodyLimit(limit int) fiber.Handler {
	if limit < 1 {
		// Config validation enforces this; reaching it is a programming error.
		panic("middleware: BodyLimit requires limit >= 1")
	}
	return func(c fiber.Ctx) error {
		if isMultipartForm(c.Get(fiber.HeaderContentType)) {
			return c.Next()
		}
		// Content-Length is the cheap check; the buffered length also covers
		// chunked bodies, which report no Content-Length.
		if c.Request().Header.ContentLength() > limit || len(c.Request().Body()) > limit {
			return apperror.NewPayloadTooLarge("request body too large")
		}
		return c.Next()
	}
}

func isMultipartForm(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), fiber.MIMEMultipartForm)
}
