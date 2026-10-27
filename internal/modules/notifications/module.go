// Package notifications owns the in-app notification feed, channel
// preferences, and durable email delivery for notifications.
package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	notificationsprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/privacy"
	notificationsservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/google/uuid"
)

// Module owns the authenticated notification and preference routes.
type Module struct {
	handler         *notificationshandler.Handler
	retentionPurger *notificationsservice.RetentionPurger
	notifier        notificationcontracts.Notifier
	privacy         *notificationsprivacy.Provider
	tenantPurger    tenancycontracts.Purger
}

var (
	_ app.RouteProvider    = (*Module)(nil)
	_ app.EventProvider    = (*Module)(nil)
	_ app.JobProvider      = (*Module)(nil)
	_ app.ScheduleProvider = (*Module)(nil)
	_ app.PrivacyProvider  = (*Module)(nil)
)

func New(handler *notificationshandler.Handler) *Module {
	return &Module{handler: handler}
}

func (m *Module) Name() string { return notificationscatalog.Owner }

// Notifier exposes the cross-module create API (used by identity's export job
// and wired in bootstrap after both modules are built).
func (m *Module) Notifier() notificationcontracts.Notifier { return m.notifier }

// RegisterPrivacy implements app.PrivacyProvider.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "notifications", Exporter: m.privacy, Eraser: m.privacy})
}

// RegisterRoutes implements app.RouteProvider. The notifications module owns
// the caller's feed and preferences plus the member-scoped send.
func (m *Module) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	m.handler.Register(api)
}

// RegisterEvents implements app.EventProvider: identity publishes
// identity.data_export.ready after the export row is ready, and this durable
// subscription creates the in-app notification (and its email, according to
// the user's preferences). The two modules stay decoupled: no late binding and
// no import of identity's internals.
func (m *Module) RegisterEvents(bus *events.Bus) {
	if err := bus.SubscribeDurable(identitycontracts.EventDataExportReady, "notifications", m.handleDataExportReady); err != nil {
		panic(fmt.Sprintf("notifications: subscribe %s: %v", identitycontracts.EventDataExportReady, err))
	}
}

func (m *Module) handleDataExportReady(ctx context.Context, event events.Event) error {
	var payload identitycontracts.DataExportReady
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return jobs.Permanent(fmt.Errorf("decode %s: %w", identitycontracts.EventDataExportReady, err))
	}
	if payload.ExportID == uuid.Nil || payload.UserID == uuid.Nil || event.TenantID == uuid.Nil {
		return jobs.Permanent(fmt.Errorf("%s: missing export, user, or tenant", identitycontracts.EventDataExportReady))
	}
	data, err := json.Marshal(map[string]string{"exportId": payload.ExportID.String()})
	if err != nil {
		return jobs.Permanent(fmt.Errorf("encode notification data: %w", err))
	}
	return m.notifier.Notify(ctx, notificationcontracts.CreateNotificationInput{
		TenantID: event.TenantID,
		UserID:   payload.UserID,
		Type:     "privacy.data_export_ready",
		Title:    "Your data export is ready",
		Body:     "Download it from your account within 7 days.",
		Data:     data,
	})
}
