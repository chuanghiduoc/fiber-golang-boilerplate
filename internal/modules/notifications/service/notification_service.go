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

	"github.com/jackc/pgx/v5"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
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

// TransactionalEmailSender can enqueue an email on a caller-owned database
// transaction. jobs.QueuedSender implements it; a plain email.Sender does not.
type TransactionalEmailSender interface {
	SendTx(ctx context.Context, tx pgx.Tx, msg email.Message) error
}

// TransactionManager runs fn inside a transaction. database.TxManager
// implements it; tests substitute a fake that models rollback.
type TransactionManager interface {
	WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// CreateResult reports which channels accepted a notification. Notification
// is nil when the in-app channel is disabled.
type CreateResult struct {
	Notification *dto.NotificationResponse
	EmailQueued  bool
}

// NotificationService is the notifications module's business surface.
type NotificationService interface {
	Create(ctx context.Context, input notificationcontracts.CreateNotificationInput) (CreateResult, error)
	Notify(ctx context.Context, input notificationcontracts.CreateNotificationInput) error
	Get(ctx context.Context, userID, id int64) (*dto.NotificationResponse, error)
	List(ctx context.Context, userID int64, limit int, startingAfter string, unreadOnly bool) ([]dto.NotificationResponse, bool, error)
	MarkRead(ctx context.Context, userID, id int64) (*dto.NotificationResponse, error)
	MarkAllRead(ctx context.Context, userID int64) (int64, error)
	UnreadCount(ctx context.Context, userID int64) (int64, error)
	ListPreferences(ctx context.Context, userID int64) ([]dto.PreferenceResponse, error)
	SetPreference(ctx context.Context, userID int64, req dto.UpdatePreferenceRequest) (*dto.PreferenceResponse, error)
}

type notificationService struct {
	repo       repository.NotificationRepository
	recipients identitycontracts.NotificationRecipientLookup
	sender     email.Sender
	txManager  TransactionManager
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
	repo repository.NotificationRepository,
	recipients identitycontracts.NotificationRecipientLookup,
	sender email.Sender,
	txManager TransactionManager,
) NotificationService {
	return &notificationService{
		repo:       repo,
		recipients: recipients,
		sender:     sender,
		txManager:  txManager,
	}
}

// Create validates the notification, applies the user's channel preferences,
// stores the in-app row, and enqueues the email channel. When a transaction
// manager and a transactional sender are wired, the row and job commit
// together: an enqueue failure rolls the row back.
func (s *notificationService) Create(ctx context.Context, input notificationcontracts.CreateNotificationInput) (CreateResult, error) {
	scopedCtx, err := resolveTenantContext(ctx, input.TenantID)
	if err != nil {
		return CreateResult{}, err
	}

	normalized, err := normalizeInput(input)
	if err != nil {
		return CreateResult{}, err
	}

	inAppEnabled, err := s.channelEnabled(scopedCtx, normalized.UserID, normalized.Type, ChannelInApp)
	if err != nil {
		return CreateResult{}, err
	}
	emailEnabled, err := s.channelEnabled(scopedCtx, normalized.UserID, normalized.Type, ChannelEmail)
	if err != nil {
		return CreateResult{}, err
	}
	if !inAppEnabled && !emailEnabled {
		return CreateResult{}, nil
	}

	var message email.Message
	if emailEnabled {
		message, err = s.buildEmailMessage(scopedCtx, normalized)
		if err != nil {
			return CreateResult{}, err
		}
	}

	params := notificationsqlc.CreateNotificationParams{
		UserID: normalized.UserID,
		Type:   normalized.Type,
		Title:  normalized.Title,
		Body:   normalized.Body,
		Data:   normalized.Data,
	}

	var created *notificationsqlc.Notification
	persist := func(repo repository.NotificationRepository, tx pgx.Tx) error {
		if inAppEnabled {
			row, createErr := repo.Create(scopedCtx, params)
			if createErr != nil {
				return createErr
			}
			created = row
		}
		if !emailEnabled {
			return nil
		}
		if txSender, ok := s.sender.(TransactionalEmailSender); ok && s.txManager != nil {
			return txSender.SendTx(scopedCtx, tx, message)
		}
		if s.sender == nil {
			return errors.New("notification email sender is not configured")
		}
		return s.sender.Send(scopedCtx, message)
	}

	switch {
	case s.txManager != nil && emailEnabled:
		err = s.txManager.WithTx(scopedCtx, func(tx pgx.Tx) error {
			return persist(s.repo.WithTx(tx), tx)
		})
	case inAppEnabled || emailEnabled:
		err = persist(s.repo, nil)
	}
	if err != nil {
		return CreateResult{}, s.createError(scopedCtx, err, normalized)
	}

	result := CreateResult{EmailQueued: emailEnabled}
	if created != nil {
		result.Notification = toNotificationResponse(created)
	}
	return result, nil
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

func (s *notificationService) buildEmailMessage(ctx context.Context, input notificationcontracts.CreateNotificationInput) (email.Message, error) {
	if s.recipients == nil {
		return email.Message{}, apperror.NewInternal("notification recipient lookup is not configured")
	}
	if s.sender == nil {
		return email.Message{}, apperror.NewInternal("notification email sender is not configured")
	}

	recipient, err := s.recipients.NotificationRecipient(ctx, input.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return email.Message{}, apperror.NewNotFound("notification recipient not found")
		}
		return email.Message{}, apperror.NewInternal("failed to resolve notification recipient")
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

func (s *notificationService) Get(ctx context.Context, userID, id int64) (*dto.NotificationResponse, error) {
	row, err := s.repo.GetByID(ctx, id, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("notification not found")
		}
		return nil, apperror.NewInternal("failed to get notification")
	}
	return toNotificationResponse(row), nil
}

func (s *notificationService) List(ctx context.Context, userID int64, limit int, startingAfter string, unreadOnly bool) ([]dto.NotificationResponse, bool, error) {
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

	responses := make([]dto.NotificationResponse, len(rows))
	for i := range rows {
		responses[i] = *toNotificationResponse(&rows[i])
	}
	return responses, hasMore, nil
}

func (s *notificationService) MarkRead(ctx context.Context, userID, id int64) (*dto.NotificationResponse, error) {
	row, err := s.repo.MarkRead(ctx, id, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("notification not found")
		}
		return nil, apperror.NewInternal("failed to mark notification as read")
	}
	return toNotificationResponse(row), nil
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

func (s *notificationService) ListPreferences(ctx context.Context, userID int64) ([]dto.PreferenceResponse, error) {
	rows, err := s.repo.ListPreferences(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to list notification preferences")
	}
	responses := make([]dto.PreferenceResponse, len(rows))
	for i := range rows {
		responses[i] = *toPreferenceResponse(&rows[i])
	}
	return responses, nil
}

func (s *notificationService) SetPreference(ctx context.Context, userID int64, req dto.UpdatePreferenceRequest) (*dto.PreferenceResponse, error) {
	notificationType := strings.TrimSpace(req.Type)
	if err := validateType(notificationType); err != nil {
		return nil, err
	}
	if req.Channel != ChannelInApp && req.Channel != ChannelEmail {
		return nil, apperror.NewBadRequest("channel must be in_app or email")
	}
	if req.Enabled == nil {
		return nil, apperror.NewBadRequest("enabled is required")
	}

	row, err := s.repo.UpsertPreference(ctx, notificationsqlc.UpsertNotificationPreferenceParams{
		UserID:  userID,
		Type:    notificationType,
		Channel: req.Channel,
		Enabled: *req.Enabled,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to save notification preference")
	}
	return toPreferenceResponse(row), nil
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

func toNotificationResponse(row *notificationsqlc.Notification) *dto.NotificationResponse {
	data := map[string]any{}
	if len(row.Data) > 0 {
		_ = json.Unmarshal(row.Data, &data)
	}
	resp := &dto.NotificationResponse{
		ID:        row.ID,
		TenantID:  row.TenantID,
		UserID:    row.UserID,
		Type:      row.Type,
		Title:     row.Title,
		Body:      row.Body,
		Data:      data,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.ReadAt.Valid {
		readAt := row.ReadAt.Time
		resp.ReadAt = &readAt
	}
	return resp
}

func toPreferenceResponse(row *notificationsqlc.NotificationPreference) *dto.PreferenceResponse {
	return &dto.PreferenceResponse{
		ID:        row.ID,
		TenantID:  row.TenantID,
		UserID:    row.UserID,
		Type:      row.Type,
		Channel:   row.Channel,
		Enabled:   row.Enabled,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
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
