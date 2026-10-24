package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMatches(t *testing.T) {
	cases := []struct {
		pattern string
		event   string
		want    bool
	}{
		{"*", "file.uploaded", true},
		{"*", "", false},
		{"file.uploaded", "file.uploaded", true},
		{"file.uploaded", "file.deleted", false},
		{"file.*", "file.uploaded", true},
		{"file.*", "file.", false},
		{"file.*", "files.uploaded", false},
		{"file.*", "file", false},
		{" user.* ", "user.registered", true},
	}
	for _, tc := range cases {
		if got := Match(tc.pattern, tc.event); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.event, got, tc.want)
		}
	}

	if !Matches([]string{"user.*", "file.uploaded"}, "file.uploaded") {
		t.Error("Matches should accept any matching pattern")
	}
	if Matches(nil, "file.uploaded") {
		t.Error("empty patterns must not match")
	}
}

func TestEventValidate(t *testing.T) {
	valid := Event{Type: "file.uploaded", Data: json.RawMessage(`{"id":1}`)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid): %v", err)
	}

	cases := []Event{
		{Type: ""},
		{Type: "File Uploaded"},
		{Type: "FILE.UPLOADED"},
		{Type: "file.uploaded", Data: json.RawMessage(`not-json`)},
		{Type: "file.uploaded", TenantID: -1},
		{Type: "file.uploaded", ID: "has space"},
		{Type: "file.uploaded", ID: "bad/id"},
	}
	for _, event := range cases {
		if err := event.Validate(); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidEvent", event, err)
		}
	}
}

func TestValidID(t *testing.T) {
	valid := []string{"evt_1", "replay.7.abc123", "A:b-c_d.9"}
	for _, value := range valid {
		if !ValidID(value) {
			t.Errorf("ValidID(%q) = false, want true", value)
		}
	}
	invalid := []string{"", "has space", "bad/id", "bad\n", strings.Repeat("a", 129)}
	for _, value := range invalid {
		if ValidID(value) {
			t.Errorf("ValidID(%q) = true, want false", value)
		}
	}
}

func TestEventNormalized(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	event := Event{Type: "file.uploaded"}.Normalized(now)
	if string(event.Data) != "{}" {
		t.Errorf("Data = %s, want {}", event.Data)
	}
	if !event.OccurredAt.Equal(now) {
		t.Errorf("OccurredAt = %v, want %v", event.OccurredAt, now)
	}
}

func TestValidType(t *testing.T) {
	valid := []string{"*", "file.*", "file.uploaded", "file_v2.uploaded", "a-b.c"}
	for _, value := range valid {
		if !ValidType(value) {
			t.Errorf("ValidType(%q) = false, want true", value)
		}
	}
	invalid := []string{"", ".*", "File.uploaded", "file uploaded", "file/*", "file.*.uploaded"}
	for _, value := range invalid {
		if ValidType(value) {
			t.Errorf("ValidType(%q) = true, want false", value)
		}
	}
}

func TestNormalizePatterns(t *testing.T) {
	got := NormalizePatterns([]string{" file.* ", "file.uploaded", "file.*", "", "Bad", "user.registered"})
	want := []string{"file.*", "file.uploaded", "user.registered"}
	if len(got) != len(want) {
		t.Fatalf("NormalizePatterns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizePatterns = %v, want %v", got, want)
		}
	}
}

func TestNoop(t *testing.T) {
	count, err := Noop{}.Dispatch(context.Background(), Event{Type: "file.uploaded"})
	if err != nil || count != 0 {
		t.Fatalf("Noop.Dispatch = (%d, %v), want (0, nil)", count, err)
	}
}
