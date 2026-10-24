package features

import (
	"strings"
	"testing"
)

// Flags are owned by modules and registered at startup. The platform tests
// register stand-ins with the same keys and defaults so they stay independent
// of any business module.
const (
	KeyUserRegistration  Key = "user_registration"
	KeyEmailVerification Key = "email_verification"
	KeyGoogleOAuth       Key = "google_oauth"
	KeyFileUploads       Key = "file_uploads"
)

func TestRegisterRejectsConflicts(t *testing.T) {
	if err := Register("featurestest",
		Definition{Key: KeyUserRegistration, Description: "test", Default: true},
	); err != nil {
		t.Fatalf("identical re-registration must be a no-op: %v", err)
	}
	cases := map[string]struct {
		owner string
		defs  []Definition
		want  string
	}{
		"other owner":   {"intruder", []Definition{{Key: KeyFileUploads, Description: "x", Default: true}}, "already owned"},
		"changed":       {"featurestest", []Definition{{Key: KeyFileUploads, Description: "test", Default: false}}, "different definition"},
		"bad key":       {"x", []Definition{{Key: "Bad-Key", Description: "x"}}, "invalid flag key"},
		"no desc":       {"x", []Definition{{Key: "x_no_desc"}}, "description"},
		"listed twice":  {"x", []Definition{{Key: "x_twice", Description: "x"}, {Key: "x_twice", Description: "x"}}, "twice"},
		"missing owner": {"", []Definition{{Key: "x_owner", Description: "x"}}, "owner is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Register(tc.owner, tc.defs...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Register error = %v, want containing %q", err, tc.want)
			}
		})
	}
	if IsKnown("x_twice") {
		t.Fatal("a rejected registration must not be applied")
	}
	if owner, _ := OwnerOf(KeyFileUploads); owner != "featurestest" {
		t.Fatalf("OwnerOf = %q", owner)
	}
}

func init() {
	if err := Register("featurestest",
		Definition{Key: KeyUserRegistration, Description: "test", Default: true},
		Definition{Key: KeyEmailVerification, Description: "test", Default: false},
		Definition{Key: KeyGoogleOAuth, Description: "test", Default: true},
		Definition{Key: KeyFileUploads, Description: "test", Default: true},
	); err != nil {
		panic(err)
	}
}
