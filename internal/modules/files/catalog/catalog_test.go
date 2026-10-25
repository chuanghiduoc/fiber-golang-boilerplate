package catalog

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// TestPublicValuesAreStable pins the files module's public identifiers.
func TestPublicValuesAreStable(t *testing.T) {
	pins := map[string]string{
		string(PermissionFilesRead):     "files:read",
		string(PermissionFilesWrite):    "files:write",
		string(PermissionFilesDelete):   "files:delete",
		string(PermissionFilesRestore):  "files:restore",
		string(CodeFileNotFound):        "file.not_found",
		string(CodeFileTooLarge):        "file.too_large",
		string(CodeFileUnsupportedType): "file.unsupported_type",
		string(CodeFileNotDeleted):      "file.not_deleted",
		string(KeyFileUploads):          "file_uploads",
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
	if _, ok := apperror.SpecFor(CodeFileNotDeleted); !ok {
		t.Error("error codes not registered")
	}
	if !features.IsKnown(KeyFileUploads) {
		t.Error("flags not registered")
	}
	if !authz.Has(authz.RoleMember, PermissionFilesWrite) {
		t.Error("users must hold files:write")
	}
	bundle, err := i18n.New("en", i18n.Source{Owner: "files", FS: Locales()})
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
