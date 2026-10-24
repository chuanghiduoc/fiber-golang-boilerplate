package catalog

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// TestPublicValuesAreStable pins identity's public identifiers: clients branch
// on codes, API keys store scopes, and operators configure flag keys.
func TestPublicValuesAreStable(t *testing.T) {
	pins := map[string]string{
		string(PermissionProfileRead):      "profile:read",
		string(PermissionProfileWrite):     "profile:write",
		string(PermissionSessionsRevoke):   "sessions:revoke",
		string(CodeAuthInvalidCredentials): "auth.invalid_credentials",
		string(CodeAuthEmailNotVerified):   "auth.email_not_verified",
		string(CodeAuthRefreshInvalid):     "auth.refresh_invalid",
		string(CodeUserEmailTaken):         "user.email_taken",
		string(CodeUserNotFound):           "user.not_found",
		string(KeyUserRegistration):        "user_registration",
		string(KeyEmailVerification):       "email_verification",
		string(KeyGoogleOAuth):             "google_oauth",
	}
	for got, want := range pins {
		if got != want {
			t.Errorf("public value changed: got %q, want %q", got, want)
		}
	}
}

func TestCatalogRegistersAndLocalizes(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	if err := Register(); err != nil {
		t.Fatalf("Register must be idempotent: %v", err)
	}
	if _, ok := apperror.SpecFor(CodeUserNotFound); !ok {
		t.Error("error codes not registered")
	}
	if !features.IsKnown(KeyGoogleOAuth) {
		t.Error("flags not registered")
	}
	if !authz.Has(authz.RoleUser, PermissionProfileWrite) {
		t.Error("users must hold profile:write")
	}
	bundle, err := i18n.New("en", i18n.Source{Owner: "identity", FS: Locales()})
	if err != nil {
		t.Fatalf("locales: %v", err)
	}
	for code := range ErrorCodes() {
		key := "errors." + string(code) + ".title"
		for _, locale := range bundle.SupportedLocales() {
			if !bundle.Has(locale, key) {
				t.Errorf("%s is missing %q", locale, key)
			}
		}
	}
}
