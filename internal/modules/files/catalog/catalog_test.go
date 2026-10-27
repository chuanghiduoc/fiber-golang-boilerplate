package catalog

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
)

// TestPublicValuesAreStable pins the files module's public identifiers.
func TestPublicValuesAreStable(t *testing.T) {
	pins := map[string]string{
		string(PermissionFilesRead):     "files:read",
		string(PermissionFilesWrite):    "files:write",
		string(PermissionFilesDelete):   "files:delete",
		string(PermissionFilesRestore):  "files:restore",
		string(CodeFileTooLarge):        "file.too_large",
		string(CodeFileUnsupportedType): "file.unsupported_type",
		string(CodeFileNotDeleted):      "file.not_deleted",
		string(CodeFileQuotaExceeded):   "file.quota_exceeded",
		string(CodeFileInfected):        "file.infected",
		string(CodeFileScanUnavailable): "file.scan_unavailable",
		string(KeyFileUploads):          "file_uploads",
	}
	for got, want := range pins {
		if got != want {
			t.Errorf("public value changed: got %q, want %q", got, want)
		}
	}
}

func TestCatalogProvidersBuildInjectedCatalogs(t *testing.T) {
	errors := testErrors()
	if _, ok := errors.SpecFor(CodeFileNotDeleted); !ok {
		t.Error("error codes not provided")
	}
	permissions := testPermissions()
	if !permissions.Has(authz.RoleMember, PermissionFilesWrite) {
		t.Error("users must hold files:write")
	}
	if !testFlags().IsKnown(KeyFileUploads) {
		t.Error("flags not provided")
	}
	bundle, err := i18n.New("en", i18n.Source{Owner: "files", FS: Locales()})
	if err != nil {
		t.Fatalf("locales: %v", err)
	}
	for _, spec := range Errors() {
		key := "errors." + string(spec.Code) + ".title"
		for _, locale := range bundle.SupportedLocales() {
			if !bundle.Has(locale, key) {
				t.Errorf("%s is missing %q", locale, key)
			}
		}
	}
}
