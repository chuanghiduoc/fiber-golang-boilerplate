package audit

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	metadataMaxDepth      = 4
	metadataMaxItems      = 50
	metadataMaxStringLen  = 512
	metadataMaxTotalBytes = 8192
)

var (
	emailPattern = regexp.MustCompile(`(?i)[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	urlPattern   = regexp.MustCompile(`(?i)^(https?://|ftp://|mailto:|wss?://)`)
)

// sensitiveMetadataKeyParts are compared case-insensitively against keys.
// Values under matching keys are replaced rather than truncated: a partial
// token or email is still a disclosure.
var sensitiveMetadataKeyParts = []string{
	"password", "passwd", "secret", "token", "authorization", "cookie",
	"api_key", "apikey", "private_key", "email", "e-mail", "url", "uri",
}

// SanitizeMetadata returns a bounded, redacted copy of metadata suitable for
// storage. It denies password/token/email/URL values regardless of key, caps
// nesting depth, collection size, string length, and total encoded size.
//
// Records are compliance artifacts, not a debug log: callers should still
// avoid putting PII or secrets in metadata in the first place.
func SanitizeMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return map[string]any{}
	}
	budget := metadataMaxTotalBytes
	return sanitizeMap(metadata, 0, &budget)
}

// MarshalMetadata encodes sanitized metadata for a JSONB column. A nil map
// becomes an empty JSON object so the NOT NULL column keeps its documented
// default shape.
func MarshalMetadata(metadata map[string]any) ([]byte, error) {
	clean := SanitizeMetadata(metadata)
	encoded, err := json.Marshal(clean)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal metadata: %w", err)
	}
	return encoded, nil
}

func sanitizeMap(input map[string]any, depth int, budget *int) map[string]any {
	if depth >= metadataMaxDepth {
		return map[string]any{"truncated": "depth"}
	}
	out := make(map[string]any, len(input))
	count := 0
	for key, value := range input {
		if count >= metadataMaxItems {
			out["truncated"] = "items"
			break
		}
		count++
		cleanKey := sanitizeString(key, budget)
		if sensitiveKey(key) {
			out[cleanKey] = "[redacted]"
			continue
		}
		out[cleanKey] = sanitizeValue(value, depth+1, budget)
	}
	return out
}

func sanitizeValue(value any, depth int, budget *int) any {
	if *budget <= 0 {
		return "[truncated]"
	}
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if emailPattern.MatchString(v) || urlPattern.MatchString(v) || strings.Contains(v, "://") {
			return "[redacted]"
		}
		return sanitizeString(v, budget)
	case bool:
		return v
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case []string:
		out := make([]string, 0, len(v))
		for i, item := range v {
			if i >= metadataMaxItems {
				break
			}
			out = append(out, sanitizeString(item, budget))
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for i, item := range v {
			if i >= metadataMaxItems {
				break
			}
			out = append(out, sanitizeValue(item, depth+1, budget))
		}
		return out
	case map[string]any:
		return sanitizeMap(v, depth, budget)
	case json.Marshaler:
		encoded, err := v.MarshalJSON()
		if err != nil {
			return "[unserializable]"
		}
		return sanitizeString(string(encoded), budget)
	default:
		return "[unsupported]"
	}
}

func sanitizeString(value string, budget *int) string {
	if len(value) > metadataMaxStringLen {
		value = value[:metadataMaxStringLen]
	}
	if *budget <= 0 {
		return "[truncated]"
	}
	if len(value) > *budget {
		value = value[:*budget]
	}
	*budget -= len(value)
	// jsonb rejects \u0000, so a NUL would fail the whole audit insert.
	return strings.ReplaceAll(strings.ToValidUTF8(value, ""), "\x00", "")
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range sensitiveMetadataKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}
