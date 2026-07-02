package pagination

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultLimit is used when a cursor request omits limit.
	DefaultLimit = 20
	// MaxLimit caps a single page to protect the database.
	MaxLimit = 100
)

// NormalizeLimit clamps a requested page size into [1, MaxLimit], defaulting
// to DefaultLimit when non-positive.
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// RowLimit safely converts a fetch count to int32 for a SQL LIMIT clause.
func RowLimit(n int) int32 {
	return clampInt32(n)
}

// EncodeCursor produces an opaque, URL-safe cursor from the sort key
// (created_at) and its monotonic tie-breaker (id). Two rows with the same
// created_at still yield distinct cursors because id is unique.
func EncodeCursor(createdAt time.Time, id int64) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor reverses EncodeCursor. The timestamp itself contains colons, so
// the id is taken from after the final colon.
func DecodeCursor(cursor string) (time.Time, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid cursor encoding")
	}
	raw := string(b)
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		return time.Time{}, 0, fmt.Errorf("malformed cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, raw[:i])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid cursor timestamp")
	}
	id, err := strconv.ParseInt(raw[i+1:], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid cursor id")
	}
	return ts, id, nil
}
