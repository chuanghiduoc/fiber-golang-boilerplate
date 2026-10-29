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
		if candidate == "" {
			continue
		}
		if candidate == "*" {
			// A wildcard requires an existing representation with a validator.
			// An empty current ETag is not a valid target to match.
			if current != "" {
				return nil
			}
			continue
		}
		if current != "" && candidate == current {
			return nil
		}
	}
	return apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
}

// splitETags splits an If-Match list without breaking a comma inside an opaque
// quoted tag. Backslash escapes inside the quoted string are respected.
func splitETags(value string) []string {
	var parts []string
	start := 0
	quoted := false
	escaped := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if quoted {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = true
		case ',':
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

// quoteETag normalizes an opaque version into a strong entity-tag.
func quoteETag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return `"` + strings.Trim(value, `"`) + `"`
}
