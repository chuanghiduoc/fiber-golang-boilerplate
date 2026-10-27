package features

import (
	"strings"
	"testing"
)

// Flags are owned by modules and contributed at the composition root. The
// platform tests build a stand-in catalog with the same keys and defaults so
// they stay independent of any business module.
const (
	KeyUserRegistration  Key = "user_registration"
	KeyEmailVerification Key = "email_verification"
	KeyGoogleOAuth       Key = "google_oauth"
	KeyFileUploads       Key = "file_uploads"
)

var testCatalog = newTestCatalog()

func newTestCatalog() *Catalog {
	catalog := NewCatalog()
	if err := catalog.Add(Provider{Owner: "featurestest", Definitions: []Definition{
		{Key: KeyUserRegistration, Description: "test", Default: true},
		{Key: KeyEmailVerification, Description: "test", Default: false},
		{Key: KeyGoogleOAuth, Description: "test", Default: true},
		{Key: KeyFileUploads, Description: "test", Default: true},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func TestAddRejectsConflicts(t *testing.T) {
	provider := Provider{Owner: "featurestest", Definitions: []Definition{
		{Key: KeyUserRegistration, Description: "test", Default: true},
	}}
	catalog := newTestCatalogWithoutSeal()
	if err := catalog.Add(provider); err != nil {
		t.Fatalf("identical re-add must be a no-op: %v", err)
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
			err := catalog.Add(Provider{Owner: tc.owner, Definitions: tc.defs})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add error = %v, want containing %q", err, tc.want)
			}
		})
	}
	if catalog.IsKnown("x_twice") {
		t.Fatal("a rejected addition must not be applied")
	}
	if owner, _ := catalog.OwnerOf(KeyFileUploads); owner != "featurestest" {
		t.Fatalf("OwnerOf = %q", owner)
	}
}

func newTestCatalogWithoutSeal() *Catalog {
	catalog := NewCatalog()
	if err := catalog.Add(Provider{Owner: "featurestest", Definitions: []Definition{
		{Key: KeyUserRegistration, Description: "test", Default: true},
		{Key: KeyEmailVerification, Description: "test", Default: false},
		{Key: KeyGoogleOAuth, Description: "test", Default: true},
		{Key: KeyFileUploads, Description: "test", Default: true},
	}}); err != nil {
		panic(err)
	}
	return catalog
}
