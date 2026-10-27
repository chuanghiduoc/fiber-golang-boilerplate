package middleware

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// Metrics records one OTel semantic-convention HTTP server-duration
// observation per request. Route is the matched template, never the raw path.
// The histogram's _count is the request count, so no separate counter exists.
func Metrics(catalog *apperror.Catalog) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		duration := time.Since(start).Seconds()
		status := effectiveStatus(catalog, c, err)
		errorType := ""
		if status >= fiber.StatusBadRequest {
			errorType = strconv.Itoa(status)
		}
		metrics.RecordHTTPServerRequest(
			c.Context(),
			httpRequestMethod(c.Method()),
			routeTemplate(c),
			status,
			errorType,
			duration,
		)

		return err
	}
}
