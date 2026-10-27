// Package handler owns HTTP transport for the notifications module.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// Route paths and the operation tags shared by the registrations.
const (
	myNotificationsPath       = "/api/v1/me/notifications"
	myNotificationPath        = "/api/v1/me/notifications/{notificationId}"
	unreadCountPath           = "/api/v1/me/notifications/unread-count"
	myNotificationPreferences = "/api/v1/me/notification-preferences"
	myNotificationPreference  = "/api/v1/me/notification-preferences/{type}/{channel}"
	memberNotificationsPath   = "/api/v1/members/{memberId}/notifications"
	notificationsTag          = "Notifications"
	// The created notification's canonical item URI is the member feed item,
	// which is the only per-notification resource in the route table.
	notificationLocationFmt = "/api/v1/me/notifications/%s"
)

// Handler binds requests, validates input, and calls the service.
type Handler struct {
	service service.NotificationService
}

// New returns a handler backed by svc.
func New(svc service.NotificationService) *Handler {
	return &Handler{service: svc}
}

// Register declares every notifications operation. The policy metadata is the
// contract test/arch and cmd/openapi read; the HTTP foundation builds the
// middleware chain from it.
func (h *Handler) Register(api *httpapi.API) {
	prepareRequestBody(api)
	bestEffort := func(action, resource string) httpapi.AuditPolicy {
		return httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{
			Mode:     audithttp.ModeBestEffort,
			Action:   action,
			Resource: resource,
		}}
	}

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: myNotificationsPath, OperationID: "listMyNotifications",
		Tags: []string{notificationsTag}, Summary: "List my notifications",
		Description: "List the authenticated member's notifications in the request tenant, newest first.",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.list)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: unreadCountPath, OperationID: "getMyUnreadNotificationCount",
		Tags: []string{notificationsTag}, Summary: "Get my unread notification count",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.unreadCount)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: myNotificationPath, OperationID: "getMyNotification",
		Tags: []string{notificationsTag}, Summary: "Get one of my notifications",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.get)

	httpapi.Register(api, httpapi.Op{
		Method: "PATCH", Path: myNotificationPath, OperationID: "updateMyNotification",
		Tags: []string{notificationsTag}, Summary: "Update one of my notifications",
		Description: "RFC 7396 merge patch on the read state: {\"read\": true} marks it read, {\"read\": false} marks it unread.",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: bestEffort("notification.read", "notification"),
	}, h.update)

	httpapi.Register(api, httpapi.Op{
		Method: "PATCH", Path: myNotificationsPath, OperationID: "updateMyNotifications",
		Tags: []string{notificationsTag}, Summary: "Mark all my notifications read",
		Description: "Bulk merge patch; the only supported document is {\"read\": true}.",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: bestEffort("notification.read_all", "notification"),
	}, h.updateAll)

	httpapi.Register(api, httpapi.Op{
		Method: "GET", Path: myNotificationPreferences, OperationID: "listMyNotificationPreferences",
		Tags: []string{notificationsTag}, Summary: "List my notification preferences",
		Description: "List explicit channel preferences. A missing row means the channel is enabled.",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsRead, RateLimit: httpapi.RateLimitRelaxed,
	}, h.listPreferences)

	httpapi.Register(api, httpapi.Op{
		Method: "PUT", Path: myNotificationPreference, OperationID: "setMyNotificationPreference",
		Tags: []string{notificationsTag}, Summary: "Set one of my notification preferences",
		Description: "Replace the enabled state for one notification type and channel at its item URI.",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: bestEffort("notification.preferences_updated", "notification_preference"),
	}, h.setPreference)

	httpapi.Register(api, httpapi.Op{
		Method: "POST", Path: memberNotificationsPath, OperationID: "createMemberNotification",
		Tags: []string{notificationsTag}, Summary: "Send a notification to a member",
		Description: "Create an in-app notification and enqueue its email channel for another member of the request tenant.",
	}, httpapi.Policy{
		Permission: notificationscatalog.PermissionNotificationsSend, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{
			Action: "notification.sent", Resource: "notification",
		}},
	}, h.createForMember)
}

// listMyNotificationsInput is the feed page request.
type listMyNotificationsInput struct {
	Limit         int    `query:"limit" default:"20" minimum:"1" maximum:"100"`
	StartingAfter string `query:"startingAfter"`
	UnreadOnly    bool   `query:"unreadOnly"`
}

// notificationIDInput addresses one notification owned by the caller.
type notificationIDInput struct {
	NotificationID uuid.UUID `path:"notificationId" format:"uuid"`
}

// updateNotificationInput is a merge patch on one notification.
type updateNotificationInput struct {
	NotificationID uuid.UUID                     `path:"notificationId" format:"uuid"`
	Body           dto.UpdateNotificationRequest `contentType:"application/merge-patch+json"`
}

// updateNotificationsInput is the bulk read patch.
type updateNotificationsInput struct {
	Body dto.UpdateNotificationsRequest `contentType:"application/merge-patch+json"`
}

// preferenceInput addresses one type/channel pair at its item URI.
// setPreferenceInput carries the item URI and the replacement state.
type setPreferenceInput struct {
	Type    string `path:"type" maxLength:"100" pattern:"^[a-z0-9][a-z0-9._-]*$"`
	Channel string `path:"channel" enum:"in_app,email"`
	Body    dto.SetPreferenceRequest
}

// createMemberNotificationInput addresses the recipient by membership and
// carries the notification body.
type createMemberNotificationInput struct {
	MemberID uuid.UUID `path:"memberId" format:"uuid"`
	Body     dto.CreateMemberNotificationRequest
}

func (h *Handler) list(ctx context.Context, in *listMyNotificationsInput) (*httpapi.ListOutput[dto.NotificationResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	items, hasMore, err := h.service.List(ctx, subject.UserID, in.Limit, in.StartingAfter, in.UnreadOnly)
	if err != nil {
		return nil, err
	}
	responses := toNotificationResponses(items)
	var cursor string
	if hasMore {
		cursor = pagination.NextCursor(responses, func(n dto.NotificationResponse) (time.Time, uuid.UUID) {
			return n.CreatedAt, n.ID
		})
	}
	return &httpapi.ListOutput[dto.NotificationResponse]{
		Body: pagination.NewList(myNotificationsPath, responses, hasMore, cursor),
	}, nil
}

func (h *Handler) unreadCount(ctx context.Context, _ *struct{}) (*httpapi.Output[dto.UnreadCountResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	count, err := h.service.UnreadCount(ctx, subject.UserID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.UnreadCountResponse]{Body: dto.UnreadCountResponse{Count: count}}, nil
}

func (h *Handler) get(ctx context.Context, in *notificationIDInput) (*httpapi.Output[dto.NotificationResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	item, err := h.service.Get(ctx, subject.UserID, in.NotificationID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.NotificationResponse]{Body: *toNotificationResponse(item)}, nil
}

func (h *Handler) update(ctx context.Context, in *updateNotificationInput) (*httpapi.Output[dto.NotificationResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	// An empty merge patch changes nothing; return the current representation.
	if !in.Body.Read.Present {
		item, err := h.service.Get(ctx, subject.UserID, in.NotificationID)
		if err != nil {
			return nil, err
		}
		return &httpapi.Output[dto.NotificationResponse]{Body: *toNotificationResponse(item)}, nil
	}
	read := !in.Body.Read.Null && in.Body.Read.Value
	item, err := h.service.SetRead(ctx, subject.UserID, in.NotificationID, read)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.NotificationResponse]{Body: *toNotificationResponse(item)}, nil
}

func (h *Handler) updateAll(ctx context.Context, in *updateNotificationsInput) (*httpapi.Output[dto.UpdatedCountResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	// The collection update only means "mark every unread notification read";
	// an explicit false or null has no meaning for the collection.
	if !in.Body.Read.Present || in.Body.Read.Null || !in.Body.Read.Value {
		return nil, apperror.NewBadRequest("read must be true")
	}
	updated, err := h.service.MarkAllRead(ctx, subject.UserID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.UpdatedCountResponse]{Body: dto.UpdatedCountResponse{Updated: updated}}, nil
}

func (h *Handler) listPreferences(ctx context.Context, _ *struct{}) (*httpapi.Output[dto.NotificationPreferencesResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := h.service.ListPreferences(ctx, subject.UserID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.NotificationPreferencesResponse]{
		Body: dto.NotificationPreferencesResponse{Preferences: toPreferenceResponses(prefs)},
	}, nil
}

func (h *Handler) setPreference(ctx context.Context, in *setPreferenceInput) (*httpapi.Output[dto.NotificationPreferenceResponse], error) {
	subject, err := requestSubject(ctx)
	if err != nil {
		return nil, err
	}
	if in.Body.Enabled == nil {
		return nil, apperror.NewValidation("enabled is required", []apperror.FieldError{
			{Path: "body.enabled", Code: "required", Message: "enabled is required"},
		})
	}
	pref, err := h.service.SetPreference(ctx, subject.UserID, service.PreferenceUpdate{
		Type: in.Type, Channel: in.Channel, Enabled: in.Body.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.NotificationPreferenceResponse]{Body: *toPreferenceResponse(pref)}, nil
}

func (h *Handler) createForMember(ctx context.Context, in *createMemberNotificationInput) (*httpapi.Created[dto.CreateNotificationResponse], error) {
	if _, err := requestSubject(ctx); err != nil {
		return nil, err
	}
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, apperror.Internal(nil, "tenant context is required")
	}

	var data json.RawMessage
	if in.Body.Data != nil {
		encoded, marshalErr := json.Marshal(in.Body.Data)
		if marshalErr != nil {
			return nil, apperror.Internal(marshalErr, "data must be a JSON object")
		}
		data = encoded
	}

	result, err := h.service.Create(ctx, notificationcontracts.CreateNotificationInput{
		TenantID: tenantID,
		UserID:   in.MemberID,
		Type:     in.Body.Type,
		Title:    in.Body.Title,
		Body:     in.Body.Body,
		Data:     data,
	})
	if err != nil {
		return nil, err
	}

	var location string
	if result.Notification != nil {
		location = fmt.Sprintf(notificationLocationFmt, result.Notification.ID)
	}
	return httpapi.NewCreated(location, dto.CreateNotificationResponse{
		Notification: toNotificationResponse(result.Notification),
		EmailQueued:  result.EmailQueued,
	}), nil
}

// requestSubject returns the authenticated subject. Every route is protected,
// so a missing subject is an authentication failure rather than a 500.
func requestSubject(ctx context.Context) (authz.Subject, error) {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok || subject.UserID == uuid.Nil {
		return authz.Subject{}, apperror.NewUnauthorized("authentication is required")
	}
	return subject, nil
}
