//go:build integration

package testutil

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Catalogs builds the same instance catalogs cmd/api builds at the
// composition root. These tests wire modules by hand rather than through
// bootstrap, so they pass the result to app.Deps and the services they build.
func Catalogs() app.Catalogs {
	catalogs := app.NewCatalogs()
	for _, provider := range []struct {
		owner  string
		specs  []apperror.Spec
		grants []authz.Grant
		defs   []features.Definition
	}{
		{identitycatalog.Owner, identitycatalog.Errors(), identitycatalog.Permissions(), identitycatalog.FeatureFlags()},
		{filescatalog.Owner, filescatalog.Errors(), filescatalog.Permissions(), filescatalog.FeatureFlags()},
		{featurescatalog.Owner, nil, featurescatalog.Permissions(), nil},
		{webhookscatalog.Owner, webhookscatalog.Errors(), webhookscatalog.Permissions(), nil},
		{apikeyscatalog.Owner, apikeyscatalog.Errors(), apikeyscatalog.Permissions(), nil},
		{notificationscatalog.Owner, nil, notificationscatalog.Permissions(), nil},
	} {
		if len(provider.specs) > 0 {
			if err := catalogs.Errors.Add(apperror.Provider{Owner: provider.owner, Specs: provider.specs}); err != nil {
				panic(err)
			}
		}
		if len(provider.grants) > 0 {
			if err := catalogs.Permissions.Add(authz.Provider{Owner: provider.owner, Grants: provider.grants}); err != nil {
				panic(err)
			}
		}
		if len(provider.defs) > 0 {
			if err := catalogs.Features.Add(features.Provider{Owner: provider.owner, Definitions: provider.defs}); err != nil {
				panic(err)
			}
		}
	}
	catalogs.Errors.Seal()
	catalogs.Permissions.Seal()
	catalogs.Features.Seal()
	return catalogs
}
