package notifications

import (
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	notificationsprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/privacy"
	notificationsrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationsservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
)

// Settings are the configuration notifications owns.
type Settings struct {
	// RetentionDays is how long a read notification is kept after it was
	// read (NOTIFICATIONS_RETENTION). Zero disables the purge (tests);
	// config validates 1..3650.
	RetentionDays int32
}

// Build constructs the notification feed, preferences, and email channel.
// Identity supplies the email recipient through the contract, and the queued
// sender writes the email job into the shared outbox in the same transaction
// as the in-app row.
// Deps are the contracts notifications consumes from identity.
type Deps struct {
	Recipients identitycontracts.NotificationRecipientLookup
}

func Build(infra app.Infra, settings Settings, deps Deps) (*Module, error) {
	recipients := deps.Recipients
	repo := notificationsrepo.NewNotificationRepository(infra.DB)
	svc := notificationsservice.NewNotificationService(
		repo, recipients, infra.Email, notificationsrepo.NewTransactor(infra.Tx, infra.Email))
	m := New(notificationshandler.New(svc))
	m.notifier = svc
	m.privacy = notificationsprivacy.New(notificationsrepo.NewPrivacyStore(infra.DB))
	m.tenantPurger = notificationsrepo.NewTenantPurger(infra.DB)
	if settings.RetentionDays > 0 {
		m.retentionPurger = notificationsservice.NewRetentionPurger(
			notificationsrepo.NewRetentionRepository(infra.DB), settings.RetentionDays)
	}
	return m, nil
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return notificationscatalog.Permissions() }

// RegisterJobs implements app.JobProvider: the worker runs the read
// notification purge (under authz.SystemContext, via the app's system
// registrar). A module built with New (tests) registers nothing.
func (m *Module) RegisterJobs(registrar jobs.Registrar) {
	if m.retentionPurger != nil {
		registrar.Register(notificationsservice.RetentionPurgeJobKind, m.retentionPurger.HandleJob)
	}
}

// retentionPurgeSchedule enqueues the read-notification purge. The scheduler
// is optional (SCHEDULER_ENABLED, default false); without it notifications
// are kept indefinitely.
var retentionPurgeSchedule = scheduler.Spec{
	Name:     "notifications.retention.purge",
	Interval: notificationsservice.RetentionPurgeInterval,
	Kind:     notificationsservice.RetentionPurgeJobKind,
}

// RegisterSchedules implements app.ScheduleProvider. The spec is a constant,
// so a registration error is a programming error and fails startup loudly.
func (m *Module) RegisterSchedules(registry *scheduler.Registry) {
	if m.retentionPurger == nil {
		return
	}
	if err := registry.Register(retentionPurgeSchedule); err != nil {
		panic(fmt.Sprintf("notifications: register %s schedule: %v", retentionPurgeSchedule.Name, err))
	}
}
