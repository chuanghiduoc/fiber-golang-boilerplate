package audit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeMetadata_DeniesSensitiveKeys(t *testing.T) {
	t.Parallel()

	clean := SanitizeMetadata(map[string]any{
		"password":      "hunter2",
		"access_token":  "abc",
		"Authorization": "Bearer x",
		"email":         "user@example.com",
		"profile_url":   "https://example.com",
		"nested": map[string]any{
			"cookie": "session=1",
			"safe":   "kept",
		},
	})

	assert.Equal(t, "[redacted]", clean["password"])
	assert.Equal(t, "[redacted]", clean["access_token"])
	assert.Equal(t, "[redacted]", clean["Authorization"])
	assert.Equal(t, "[redacted]", clean["email"])
	assert.Equal(t, "[redacted]", clean["profile_url"])
	nested, ok := clean["nested"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "[redacted]", nested["cookie"])
	assert.Equal(t, "kept", nested["safe"])
}

// jsonb rejects \u0000 and PostgreSQL rejects invalid UTF-8, so either in a
// client-controlled value would fail the audit insert.
func TestSanitizeMetadata_StripsNULAndInvalidUTF8(t *testing.T) {
	t.Parallel()

	clean := SanitizeMetadata(map[string]any{"agent\x00": "bad\xff\x00value", "list": []string{"a\x00b"}})

	assert.Equal(t, "badvalue", clean["agent"])
	assert.Equal(t, []string{"ab"}, clean["list"])
}

func TestPgText_MakesClientTextStorable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "bad�ua", pgText("bad\xff\x00ua"))
	assert.Equal(t, "", pgText("\x00"))
	assert.Nil(t, nullableString("\x00"))
}

func TestSanitizeMetadata_RedactsValueShapes(t *testing.T) {
	t.Parallel()

	clean := SanitizeMetadata(map[string]any{
		"note":   "reach me at user@example.com",
		"target": "https://internal.example/path",
		"ftp":    "ftp://files.example/x",
		"plain":  "no pii here",
	})

	assert.Equal(t, "[redacted]", clean["note"])
	assert.Equal(t, "[redacted]", clean["target"])
	assert.Equal(t, "[redacted]", clean["ftp"])
	assert.Equal(t, "no pii here", clean["plain"])
}

func TestSanitizeMetadata_CapsDepthAndSize(t *testing.T) {
	t.Parallel()

	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"e": "too deep"}}}}}
	clean := SanitizeMetadata(deep)
	level1 := clean["a"].(map[string]any)
	level2 := level1["b"].(map[string]any)
	level3 := level2["c"].(map[string]any)
	assert.Equal(t, map[string]any{"truncated": "depth"}, level3["d"])

	long := strings.Repeat("x", metadataMaxStringLen+50)
	cleanLong := SanitizeMetadata(map[string]any{"value": long})
	assert.Len(t, cleanLong["value"], metadataMaxStringLen)

	many := make(map[string]any, metadataMaxItems+10)
	for i := 0; i < metadataMaxItems+10; i++ {
		many[string(rune('a'+i%26))+strings.Repeat("k", i%7)] = "v"
	}
	cleanMany := SanitizeMetadata(many)
	assert.LessOrEqual(t, len(cleanMany), metadataMaxItems+1)
}

func TestSanitizeMetadata_BoundsEncodedSize(t *testing.T) {
	t.Parallel()

	metadata := make(map[string]any, metadataMaxItems)
	for i := 0; i < metadataMaxItems; i++ {
		metadata["key"+string(rune('a'+i%26))+strings.Repeat("z", i)] = strings.Repeat("v", metadataMaxStringLen)
	}
	encoded, err := MarshalMetadata(metadata)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), metadataMaxTotalBytes+2048,
		"encoded metadata must stay near the byte budget")
}
