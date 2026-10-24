package pagination

import (
	"encoding/base64"
	"fmt"
	"math"
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

// Cursor queries always compare `(created_at, id) < (@cursor_created_at,
// @cursor_id)` instead of `NOT @has_cursor OR ...`: under a generic plan the
// OR form can only filter, so deep pages scanned every newer row, while the
// plain row comparison is an index condition. The first page therefore binds
// an upper bound above every row: ('infinity', MaxInt64).

// CreatedAtParam returns the cursor timestamp, or +infinity on the first page.
func (c DBCursor) CreatedAtParam() pgtype.Timestamptz {
	if !c.HasCursor {
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	}
	return pgtype.Timestamptz{Time: c.CreatedAt, Valid: true}
}

// IDParam returns the cursor id, or math.MaxInt64 on the first page.
func (c DBCursor) IDParam() int64 {
	if !c.HasCursor {
		return math.MaxInt64
	}
	return c.ID
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

// NextCursor returns the opaque startingAfter cursor for the page following
// items, built from the last item's sort key and id, or "" for an empty page.
// key extracts (created_at, id), or the list's equivalent sort timestamp.
func NextCursor[T any](items []T, key func(T) (time.Time, int64)) string {
	if len(items) == 0 {
		return ""
	}
	sortedAt, id := key(items[len(items)-1])
	return EncodeCursor(sortedAt, id)
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
