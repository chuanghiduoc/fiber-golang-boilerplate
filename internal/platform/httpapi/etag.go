package httpapi

import (
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// ETagHeader is the response/request header used for optimistic concurrency.
const ETagHeader = "ETag"

// IfMatchHeader is the conditional request header for updates.
const IfMatchHeader = "If-Match"

// CheckIfMatchValue enforces If-Match for a huma handler: read the header into
// the input DTO (`header:"If-Match"`) and call this with the resource's current
// validator. `*` matches any existing representation. If-Match uses the strong
// comparison required by RFC 9110.
func CheckIfMatchValue(ifMatch, currentETag string) error {
	raw := strings.TrimSpace(ifMatch)
	if raw == "" {
		return nil
	}
	current := quoteETag(currentETag)
	for _, candidate := range splitETags(raw) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return nil
		}
		if candidate == current {
			return nil
		}
	}
	return apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
}

// splitETags splits a list while tolerating a comma inside an opaque quoted tag
// far enough for our generated validators (which never contain commas).
func splitETags(value string) []string {
	return strings.Split(value, ",")
}

// quoteETag normalizes an opaque version into a strong entity-tag.
func quoteETag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.TrimPrefix(value, "W/")
	return `"` + strings.Trim(value, `"`) + `"`
}
