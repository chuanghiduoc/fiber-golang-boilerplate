package notifications

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	notificationsrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationsservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// Build constructs the notification feed, preferences, and email channel.
// Identity supplies the email recipient through the contract, and the queued
// sender writes the email job into the shared outbox in the same transaction
// as the in-app row.
func Build(infra app.Infra, recipients identitycontracts.NotificationRecipientLookup) *Module {
	repo := notificationsrepo.NewNotificationRepository(infra.Pool)
	svc := notificationsservice.NewNotificationService(repo, recipients, infra.Email, infra.Tx)
	return New(notificationshandler.New(svc))
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return notificationscatalog.Permissions() }
