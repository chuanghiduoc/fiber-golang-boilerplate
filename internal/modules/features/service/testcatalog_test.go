package service

import (
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Flag keys are owned by identity and files, which this module cannot import.
// The test catalog contributes identical stand-ins under the same owners so
// the admin service sees a realistic catalog.
const (
	testKeyUserRegistration features.Key = "user_registration"
	testKeyGoogleOAuth      features.Key = "google_oauth"
	testKeyFileUploads      features.Key = "file_uploads"
)

var (
	testCatalog = newTestFeatureCatalog()
	testAuthz   = newTestAuthzCatalog()
)

func newTestFeatureCatalog() *features.Catalog {
	catalog := features.NewCatalog()
	if err := catalog.Add(features.Provider{Owner: "identity", Definitions: []features.Definition{
		{Key: testKeyUserRegistration, Description: "Allow new users to register through the public API.", Default: true},
		{Key: testKeyGoogleOAuth, Description: "Expose Google OAuth login and callback routes (still requires Google credentials).", Default: true},
	}}); err != nil {
		panic(err)
	}
	if err := catalog.Add(features.Provider{Owner: "files", Definitions: []features.Definition{
		{Key: testKeyFileUploads, Description: "Allow authenticated file uploads.", Default: true},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func newTestAuthzCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: featurescatalog.Owner, Grants: featurescatalog.Permissions()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func testAuthorizer() authz.Authorizer { return authz.NewAuthorizer(testAuthz) }

func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := apperror.PlatformCatalog().Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
