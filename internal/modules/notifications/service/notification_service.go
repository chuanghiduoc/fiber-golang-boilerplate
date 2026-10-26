// Package service owns notification delivery rules: channel preferences,
// tenant/user scoping, in-app persistence, and durable email enqueue.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const (
	// ChannelInApp is the feed channel backed by notifications rows.
	ChannelInApp = "in_app"
	// ChannelEmail is the durable email channel.
	ChannelEmail = "email"

	maxTypeLen    = 100
	maxTitleLen   = 255
	maxBodyLen    = 10000
	maxDataBytes  = 64 * 1024
	invalidType   = "invalid notification type"
	invalidTitle  = "title is required"
	invalidBody   = "body is required"
	invalidData   = "data must be a JSON object"
	invalidTenant = "tenant context is required"
	invalidText   = "title, body, and data must be valid UTF-8 without NUL characters"
)

var notificationTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// NotificationService is the notifications module's business surface.
type NotificationService interface {
	Create(ctx context.Context, input notificationcontracts.CreateNotificationInput) (CreateResult, error)
	Notify(ctx context.Context, input notificationcontracts.CreateNotificationInput) error
	Get(ctx context.Context, userID, id int64) (*Notification, error)
	List(ctx context.Context, userID int64, limit int, startingAfter string, unreadOnly bool) ([]Notification, bool, error)
	MarkRead(ctx context.Context, userID, id int64) (*Notification, error)
	MarkAllRead(ctx context.Context, userID int64) (int64, error)
	UnreadCount(ctx context.Context, userID int64) (int64, error)
	ListPreferences(ctx context.Context, userID int64) ([]Preference, error)
	SetPreference(ctx context.Context, userID int64, update PreferenceUpdate) (*Preference, error)
}

type notificationService struct {
	repo       NotificationRepository
	recipients identitycontracts.NotificationRecipientLookup
	sender     Mailer
	tx         Transactor
}

// Notify implements notificationcontracts.Notifier for other modules. A notification
// suppressed by the user's channel preferences is a successful no-op.
func (s *notificationService) Notify(ctx context.Context, input notificationcontracts.CreateNotificationInput) error {
	_, err := s.Create(ctx, input)
	return err
}

var (
	_ NotificationService            = (*notificationService)(nil)
	_ notificationcontracts.Notifier = (*notificationService)(nil)
)

func NewNotificationService(
	repo NotificationRepository,
	recipients identitycontracts.NotificationRecipientLookup,
	sender Mailer,
	tx Transactor,
) NotificationService {
	return &notificationService{
		repo:       repo,
		recipients: recipients,
		sender:     sender,
		tx:         tx,
	}
}

// Create validates the notification, applies the user's channel preferences,
// stores the in-app row, and enqueues the email channel. When a transactor is
// wired, the row and job commit together: an enqueue failure rolls the row
// back.
func (s *notificationService) Create(ctx context.Context, input notificationcontracts.CreateNotificationInput) (CreateResult, error) {
	scopedCtx, err := resolveTenantContext(ctx, input.TenantID)
	if err != nil {
		return CreateResult{}, err
	}

	normalized, err := normalizeInput(input)
	if err != nil {
		return CreateResult{}, err
	}

	// Resolve the recipient on every path, not only when email is on: the
	// in-app row has the same (tenant_id, user_id) membership foreign key, so a
	// removed or suspended member must be a 404 before any write instead of an
	// FK violation surfacing as a 500.
	recipient, err := s.resolveRecipient(scopedCtx, normalized.UserID)
	if err != nil {
		return CreateResult{}, err
	}

	channels, err := s.enabledChannels(scopedCtx, normalized)
	if err != nil {
		return CreateResult{}, err
	}
	if !channels.inApp && !channels.email {
		// Both channels are switched off: suppression is a successful no-op.
		return CreateResult{}, nil
	}

	var message email.Message
	if channels.email {
		message, err = s.buildEmailMessage(normalized, recipient)
		if err != nil {
			return CreateResult{}, err
		}
	}

	in := NewNotification{
		UserID: normalized.UserID,
		Type:   normalized.Type,
		Title:  normalized.Title,
		Body:   normalized.Body,
		Data:   normalized.Data,
	}

	var created *Notification
	if s.tx != nil && channels.email {
		err = s.tx.InTx(scopedCtx, func(ctx context.Context, tx TxRepos) error {
			var txErr error
			created, txErr = s.deliver(ctx, tx.Notifications, tx.Mail, channels, in, message)
			return txErr
		})
	} else {
		created, err = s.deliver(scopedCtx, s.repo, s.sender, channels, in, message)
	}
	if err != nil {
		return CreateResult{}, s.createError(scopedCtx, err, normalized)
	}

	return CreateResult{Notification: created, EmailQueued: channels.email}, nil
}

// deliveryChannels are the channels a notification is delivered on after the
// recipient's preferences are applied.
type deliveryChannels struct {
	inApp bool
	email bool
}

func (s *notificationService) enabledChannels(ctx context.Context, input notificationcontracts.CreateNotificationInput) (deliveryChannels, error) {
	inApp, err := s.channelEnabled(ctx, input.UserID, input.Type, ChannelInApp)
	if err != nil {
		return deliveryChannels{}, err
	}
	emailOn, err := s.channelEnabled(ctx, input.UserID, input.Type, ChannelEmail)
	if err != nil {
		return deliveryChannels{}, err
	}
	return deliveryChannels{inApp: inApp, email: emailOn}, nil
}

// deliver stores the in-app row and enqueues the email on the enabled
// channels. mail is the transaction-bound sender, or the plain sender when the
// row and email are not committed together.
func (s *notificationService) deliver(
	ctx context.Context,
	repo NotificationRepository,
	mail Mailer,
	channels deliveryChannels,
	in NewNotification,
	message email.Message,
) (*Notification, error) {
	var created *Notification
	if channels.inApp {
		row, err := repo.Create(ctx, in)
		if err != nil {
			return nil, err
		}
		created = row
	}
	if channels.email {
		if err := sendEmail(ctx, mail, message); err != nil {
			return nil, err
		}
	}
	return created, nil
}

func sendEmail(ctx context.Context, mail Mailer, message email.Message) error {
	if mail == nil {
		return errors.New("notification email sender is not configured")
	}
	return mail.Send(ctx, message)
}

// createError preserves domain errors and hides persistence/queue internals
// behind a public 500 detail while logging the cause.
func (s *notificationService) createError(ctx context.Context, err error, input notificationcontracts.CreateNotificationInput) error {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return err
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return apperror.NewNotFound("notification recipient not found")
	}
	slog.Error("failed to create notification",
		slog.Int64("tenant_id", tenant.EffectiveID(ctx)),
		slog.Int64("user_id", input.UserID),
		slog.String("type", input.Type),
		slog.Any("error", err),
	)
	return apperror.NewInternal("failed to create notification")
}

// resolveRecipient resolves the recipient through the identity contract, which
// only returns an active member of the context tenant.
func (s *notificationService) resolveRecipient(ctx context.Context, userID int64) (identitycontracts.NotificationRecipient, error) {
	if s.recipients == nil {
		return identitycontracts.NotificationRecipient{}, apperror.NewInternal("notification recipient lookup is not configured")
	}
	recipient, err := s.recipients.NotificationRecipient(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.NotificationRecipient{}, apperror.NewNotFound("notification recipient not found")
		}
		return identitycontracts.NotificationRecipient{}, apperror.NewInternal("failed to resolve notification recipient")
	}
	return recipient, nil
}

func (s *notificationService) buildEmailMessage(input notificationcontracts.CreateNotificationInput, recipient identitycontracts.NotificationRecipient) (email.Message, error) {
	if s.sender == nil {
		return email.Message{}, apperror.NewInternal("notification email sender is not configured")
	}
	if strings.TrimSpace(recipient.Email) == "" {
		return email.Message{}, apperror.NewInternal("notification recipient has no email address")
	}

	escapedBody := html.EscapeString(input.Body)
	escapedBody = strings.ReplaceAll(escapedBody, "\n", "<br>")
	return email.Message{
		To:      []string{recipient.Email},
		Subject: input.Title,
		Body:    input.Title + "\n\n" + input.Body,
		HTML:    fmt.Sprintf("<h1>%s</h1><p>%s</p>", html.EscapeString(input.Title), escapedBody),
	}, nil
}

func (s *notificationService) channelEnabled(ctx context.Context, userID int64, notificationType, channel string) (bool, error) {
	pref, err := s.repo.GetPreference(ctx, userID, notificationType, channel)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			// No explicit preference means the channel is enabled.
			return true, nil
		}
		return false, apperror.NewInternal("failed to read notification preferences")
	}
	return pref.Enabled, nil
}

func (s *notificationService) Get(ctx context.Context, userID, id int64) (*Notification, error) {
	row, err := s.repo.GetByID(ctx, id, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("notification not found")
		}
		return nil, apperror.NewInternal("failed to get notification")
	}
	return row, nil
}

func (s *notificationService) List(ctx context.Context, userID int64, limit int, startingAfter string, unreadOnly bool) ([]Notification, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	rows, err := s.repo.ListCursor(ctx, userID, cur, unreadOnly)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list notifications")
	}

	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}
	return rows, hasMore, nil
}

func (s *notificationService) MarkRead(ctx context.Context, userID, id int64) (*Notification, error) {
	row, err := s.repo.MarkRead(ctx, id, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("notification not found")
		}
		return nil, apperror.NewInternal("failed to mark notification as read")
	}
	return row, nil
}

func (s *notificationService) MarkAllRead(ctx context.Context, userID int64) (int64, error) {
	updated, err := s.repo.MarkAllRead(ctx, userID)
	if err != nil {
		return 0, apperror.NewInternal("failed to mark notifications as read")
	}
	return updated, nil
}

func (s *notificationService) UnreadCount(ctx context.Context, userID int64) (int64, error) {
	count, err := s.repo.CountUnread(ctx, userID)
	if err != nil {
		return 0, apperror.NewInternal("failed to count unread notifications")
	}
	return count, nil
}

func (s *notificationService) ListPreferences(ctx context.Context, userID int64) ([]Preference, error) {
	rows, err := s.repo.ListPreferences(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to list notification preferences")
	}
	return rows, nil
}

func (s *notificationService) SetPreference(ctx context.Context, userID int64, update PreferenceUpdate) (*Preference, error) {
	notificationType := strings.TrimSpace(update.Type)
	if err := validateType(notificationType); err != nil {
		return nil, err
	}
	if update.Channel != ChannelInApp && update.Channel != ChannelEmail {
		return nil, apperror.NewBadRequest("channel must be in_app or email")
	}
	if update.Enabled == nil {
		return nil, apperror.NewBadRequest("enabled is required")
	}

	row, err := s.repo.UpsertPreference(ctx, NewPreference{
		UserID:  userID,
		Type:    notificationType,
		Channel: update.Channel,
		Enabled: *update.Enabled,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to save notification preference")
	}
	return row, nil
}

func resolveTenantContext(ctx context.Context, explicit int64) (context.Context, error) {
	if explicit < 0 {
		return nil, apperror.NewBadRequest(invalidTenant)
	}

	contextTenant := tenant.IDFromContext(ctx)
	effective := tenant.EffectiveID(ctx)
	if explicit > 0 {
		if contextTenant != 0 && explicit != contextTenant {
			return nil, apperror.NewForbidden("tenant mismatch")
		}
		effective = explicit
	}
	if effective <= 0 {
		return nil, apperror.NewInternal(invalidTenant)
	}
	if contextTenant != effective {
		ctx = tenant.WithID(ctx, effective)
	}
	return ctx, nil
}

func normalizeInput(input notificationcontracts.CreateNotificationInput) (notificationcontracts.CreateNotificationInput, error) {
	input.Type = strings.TrimSpace(input.Type)
	input.Title = strings.TrimSpace(input.Title)
	input.Body = strings.TrimSpace(input.Body)
	if input.UserID <= 0 {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest("userId is required")
	}
	if err := validateType(input.Type); err != nil {
		return notificationcontracts.CreateNotificationInput{}, err
	}
	// PostgreSQL text rejects NUL bytes and invalid UTF-8; reject them here as
	// a 400 instead of surfacing the insert failure as a 500.
	if !storableText(input.Title) || !storableText(input.Body) {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest(invalidText)
	}
	if input.Title == "" {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest(invalidTitle)
	}
	if len([]rune(input.Title)) > maxTitleLen {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest("title is too long")
	}
	if input.Body == "" {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest(invalidBody)
	}
	if len([]rune(input.Body)) > maxBodyLen {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest("body is too long")
	}

	data := bytes.TrimSpace(input.Data)
	if len(data) == 0 {
		data = []byte("{}")
	}
	if len(data) > maxDataBytes {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest("data is too large")
	}
	// encoding/json silently replaces invalid UTF-8, so check the raw bytes;
	// jsonb also rejects the \u0000 escape, which only a decoded walk can tell
	// apart from an escaped backslash followed by "u0000".
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest(invalidData)
	}
	if !utf8.Valid(data) || jsonContainsNUL(object) {
		return notificationcontracts.CreateNotificationInput{}, apperror.NewBadRequest(invalidText)
	}
	input.Data = json.RawMessage(data)
	return input, nil
}

// storableText reports whether s can be stored in a PostgreSQL text column.
func storableText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// jsonContainsNUL reports whether any decoded string or object key contains
// a NUL character.
func jsonContainsNUL(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case map[string]any:
		for key, item := range v {
			if strings.ContainsRune(key, 0) || jsonContainsNUL(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if jsonContainsNUL(item) {
				return true
			}
		}
	}
	return false
}

func validateType(notificationType string) error {
	if notificationType == "" || len(notificationType) > maxTypeLen || !notificationTypePattern.MatchString(notificationType) {
		return apperror.NewBadRequest(invalidType)
	}
	return nil
}

// buildCursor normalizes the page size and decodes an optional cursor.
func buildCursor(limit int, startingAfter string) (cur pagination.DBCursor, pageSize int, err error) {
	pageSize = pagination.NormalizeLimit(limit)
	cur = pagination.DBCursor{Limit: pagination.RowLimit(pageSize + 1)}
	if startingAfter != "" {
		ts, id, decErr := pagination.DecodeCursor(startingAfter)
		if decErr != nil {
			return pagination.DBCursor{}, 0, apperror.NewBadRequest("invalid cursor")
		}
		cur.HasCursor = true
		cur.CreatedAt = ts
		cur.ID = id
	}
	return cur, pageSize, nil
}
