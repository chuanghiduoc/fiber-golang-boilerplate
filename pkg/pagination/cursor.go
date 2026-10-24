package pagination

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// DefaultLimit is used when a cursor request omits limit.
	DefaultLimit = 20
	// MaxLimit caps a single page to protect the database.
	MaxLimit = 100
)

// DBCursor carries decoded cursor-pagination parameters into repository
// queries. Limit should already include the extra row used to detect the next
// page. It is shared by repositories in any module; the HTTP layer owns the
// wire format and this type owns the database hand-off.
type DBCursor struct {
	HasCursor bool
	CreatedAt time.Time
	ID        int64
	Limit     int32
}

// CreatedAtParam returns the cursor timestamp as a nullable timestamptz so
// the first page does not filter on a zero time.
func (c DBCursor) CreatedAtParam() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: c.CreatedAt, Valid: c.HasCursor}
}

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
	// Bound attacker-controlled input before allocating the decoded buffer.
	if cursor == "" || len(cursor) > 512 {
		return time.Time{}, 0, fmt.Errorf("invalid cursor length")
	}
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
