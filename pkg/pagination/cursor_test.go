package pagination

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNextCursor(t *testing.T) {
	type row struct {
		at time.Time
		id uuid.UUID
	}
	key := func(r row) (time.Time, uuid.UUID) { return r.at, r.id }

	if got := NextCursor([]row(nil), key); got != "" {
		t.Fatalf("empty page cursor = %q, want empty", got)
	}
	last := time.Date(2026, 4, 30, 22, 28, 27, 356_000_000, time.UTC)
	rows := []row{{at: last.Add(time.Hour), id: testID(9)}, {at: last, id: testID(7)}}
	if got, want := NextCursor(rows, key), EncodeCursor(last, testID(7)); got != want {
		t.Fatalf("cursor = %q, want last row's %q", got, want)
	}
}

// encodeRaw base64url-encodes a raw payload to build malformed cursors for tests.
func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestEncodeDecodeCursor_RoundTrip(t *testing.T) {
	// Microsecond precision (PostgreSQL timestamptz granularity) with a colon-rich
	// timestamp to exercise the LastIndex(":") split.
	ts := time.Date(2026, 4, 30, 22, 28, 27, 356_000_000, time.UTC)
	cursor := EncodeCursor(ts, testID(12345))

	gotTS, gotID, err := DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeCursor failed: %v", err)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", gotTS, ts)
	}
	if gotID != testID(12345) {
		t.Errorf("id = %d, want 12345", gotID)
	}
}

func TestEncodeCursor_NormalisesToUTC(t *testing.T) {
	loc := time.FixedZone("UTC+7", 7*3600)
	ts := time.Date(2026, 4, 30, 22, 28, 27, 0, loc)
	_, _, err := DecodeCursor(EncodeCursor(ts, testID(1)))
	if err != nil {
		t.Fatalf("cursor from non-UTC time should decode: %v", err)
	}
}

func TestDecodeCursor_Errors(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
	}{
		{"invalid base64", "!!!not-base64!!!"},
		{"missing colon", encodeRaw("no-colon-here")},
		{"bad timestamp", encodeRaw("not-a-time:1")},
		{"bad id", encodeRaw("2026-04-30T22:28:27Z:notanumber")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := DecodeCursor(tc.cursor); err == nil {
				t.Errorf("expected error for %q", tc.cursor)
			}
		})
	}
}

func TestNormalizeLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultLimit},
		{-5, DefaultLimit},
		{10, 10},
		{MaxLimit, MaxLimit},
		{MaxLimit + 50, MaxLimit},
	}
	for _, c := range cases {
		if got := NormalizeLimit(c.in); got != c.want {
			t.Errorf("NormalizeLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestRowLimit(t *testing.T) {
	if got := RowLimit(21); got != 21 {
		t.Errorf("RowLimit(21) = %d, want 21", got)
	}
}

func TestDBCursorParams(t *testing.T) {
	first := DBCursor{Limit: 21}
	if at := first.CreatedAtParam(); !at.Valid || at.InfinityModifier != pgtype.Infinity {
		t.Fatalf("first page CreatedAtParam = %+v, want valid +infinity", at)
	}
	if id := first.IDParam(); id != MaxUUID {
		t.Fatalf("first page IDParam = %d, want MaxInt64", id)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	next := DBCursor{HasCursor: true, CreatedAt: at, ID: testID(42)}
	if got := next.CreatedAtParam(); !got.Valid || got.InfinityModifier != pgtype.Finite || !got.Time.Equal(at) {
		t.Fatalf("cursor CreatedAtParam = %+v, want %v", got, at)
	}
	if id := next.IDParam(); id != testID(42) {
		t.Fatalf("cursor IDParam = %d, want 42", id)
	}
}
