//go:build integration

package integration_test

import (
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
)

// These tests wire modules by hand rather than through cmd/api, so they
// register every module catalog up front exactly as app.RegisterCatalogs does
// at startup (API-key scopes such as files:read, flags, and error codes).
func init() {
	for _, register := range []func() error{
		identitycatalog.Register,
		filescatalog.Register,
		featurescatalog.Register,
		webhookscatalog.Register,
		apikeyscatalog.Register,
		notificationscatalog.Register,
	} {
		if err := register(); err != nil {
			panic(err)
		}
	}
}
