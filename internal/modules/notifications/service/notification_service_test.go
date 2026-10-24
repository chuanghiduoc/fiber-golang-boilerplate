package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func testContext(tenantID int64) context.Context {
	return tenant.WithID(context.Background(), tenantID)
}

func newTestService(repo *mockRepository, sender *mockEmailSender) NotificationService {
	recipients := &mockRecipientLookup{recipients: map[int64]identitycontracts.NotificationRecipient{
		10: {UserID: 10, Email: "user10@example.com", Name: "User Ten"},
		11: {UserID: 11, Email: "user11@example.com", Name: "User Eleven"},
	}}
	return NewNotificationService(repo, recipients, sender, &mockTxManager{repo: repo})
}

func seedNotification(repo *mockRepository, tenantID, userID int64, title string, read bool, createdAt time.Time) *notificationsqlc.Notification {
	readAt := pgtype.Timestamptz{}
	if read {
		readAt = pgtype.Timestamptz{Time: createdAt.Add(time.Minute), Valid: true}
	}
	row := &notificationsqlc.Notification{
		ID:        repo.nextNotifID,
		TenantID:  tenantID,
		UserID:    userID,
		Type:      "system.info",
		Title:     title,
		Body:      title + " body",
		Data:      []byte(`{}`),
		ReadAt:    readAt,
		CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
	}
	repo.notifications[row.ID] = row
	repo.nextNotifID++
	return row
}

func TestCreateStoresInAppAndQueuesEmail(t *testing.T) {
	repo := newMockRepository()
	sender := &mockEmailSender{}
	svc := newTestService(repo, sender)

	result, err := svc.Create(testContext(1), notificationcontracts.CreateNotificationInput{
		UserID: 10,
		Type:   "security.alert",
		Title:  "Security <alert>",
		Body:   "Line one\nLine two",
		Data:   json.RawMessage(`{"url":"https://example.com/a?b=1"}`),
	})
	require.NoError(t, err)
	require.NotNil(t, result.Notification)
	assert.True(t, result.EmailQueued)
	assert.Equal(t, int64(1), result.Notification.TenantID)
	assert.Equal(t, int64(10), result.Notification.UserID)
	assert.Equal(t, "security.alert", result.Notification.Type)
	assert.Equal(t, map[string]any{"url": "https://example.com/a?b=1"}, result.Notification.Data)
	assert.Nil(t, result.Notification.ReadAt)
	assert.Len(t, repo.notifications, 1)

	require.Len(t, sender.messages, 1)
	msg := sender.messages[0]
	assert.Equal(t, []string{"user10@example.com"}, msg.To)
	assert.Equal(t, "Security <alert>", msg.Subject)
	assert.Contains(t, msg.Body, "Line one")
	assert.Contains(t, msg.Body, "Line two")
	assert.Contains(t, msg.HTML, "&lt;alert&gt;")
	assert.Contains(t, msg.HTML, "Line one<br>Line two")
	assert.NotContains(t, msg.HTML, "<alert>")
	assert.Equal(t, 1, sender.txCalls, "production path enqueues on the notification transaction")
}

func TestCreateHonorsChannelPreferences(t *testing.T) {
	repo := newMockRepository()
	sender := &mockEmailSender{}
	svc := newTestService(repo, sender)
	ctx := testContext(1)

	_, err := svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{Type: "security.alert", Channel: ChannelInApp, Enabled: new(false)})
	require.NoError(t, err)

	result, err := svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "security.alert", Title: "Email only", Body: "Body",
	})
	require.NoError(t, err)
	assert.Nil(t, result.Notification)
	assert.True(t, result.EmailQueued)
	assert.Empty(t, repo.notifications)
	require.Len(t, sender.messages, 1)

	_, err = svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{Type: "security.alert", Channel: ChannelInApp, Enabled: new(true)})
	require.NoError(t, err)
	_, err = svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{Type: "security.alert", Channel: ChannelEmail, Enabled: new(false)})
	require.NoError(t, err)

	result, err = svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "security.alert", Title: "In-app only", Body: "Body",
	})
	require.NoError(t, err)
	assert.NotNil(t, result.Notification)
	assert.False(t, result.EmailQueued)
	assert.Len(t, repo.notifications, 1)
	assert.Len(t, sender.messages, 1, "disabled email channel must not enqueue")

	_, err = svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{Type: "security.alert", Channel: ChannelInApp, Enabled: new(false)})
	require.NoError(t, err)
	result, err = svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "security.alert", Title: "Suppressed", Body: "Body",
	})
	require.NoError(t, err)
	assert.Nil(t, result.Notification)
	assert.False(t, result.EmailQueued)
	assert.Len(t, repo.notifications, 1)
	assert.Len(t, sender.messages, 1)
}

func TestCreateRollsBackWhenEmailEnqueueFails(t *testing.T) {
	repo := newMockRepository()
	sender := &mockEmailSender{fail: errors.New("queue unavailable")}
	svc := newTestService(repo, sender)

	result, err := svc.Create(testContext(1), notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "security.alert", Title: "Atomic", Body: "Body",
	})
	require.Error(t, err)
	assert.Empty(t, result.Notification)
	assert.False(t, result.EmailQueued)
	assert.Empty(t, repo.notifications, "enqueue failure must roll the in-app row back")

	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 500, appErr.Status)
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input notificationcontracts.CreateNotificationInput
		ctx   context.Context
		code  int
	}{
		{
			name:  "missing user",
			input: notificationcontracts.CreateNotificationInput{Type: "system.info", Title: "Title", Body: "Body"},
			ctx:   testContext(1),
			code:  400,
		},
		{
			name:  "empty title",
			input: notificationcontracts.CreateNotificationInput{UserID: 10, Type: "system.info", Body: "Body"},
			ctx:   testContext(1),
			code:  400,
		},
		{
			name:  "uppercase type",
			input: notificationcontracts.CreateNotificationInput{UserID: 10, Type: "System.Info", Title: "Title", Body: "Body"},
			ctx:   testContext(1),
			code:  400,
		},
		{
			name: "data array",
			input: notificationcontracts.CreateNotificationInput{
				UserID: 10, Type: "system.info", Title: "Title", Body: "Body", Data: json.RawMessage(`[1]`),
			},
			ctx:  testContext(1),
			code: 400,
		},
		{
			name: "data null",
			input: notificationcontracts.CreateNotificationInput{
				UserID: 10, Type: "system.info", Title: "Title", Body: "Body", Data: json.RawMessage(`null`),
			},
			ctx:  testContext(1),
			code: 400,
		},
		{
			name: "tenant mismatch",
			input: notificationcontracts.CreateNotificationInput{
				TenantID: 2, UserID: 10, Type: "system.info", Title: "Title", Body: "Body",
			},
			ctx:  testContext(1),
			code: 403,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newMockRepository()
			svc := newTestService(repo, &mockEmailSender{})
			_, err := svc.Create(tt.ctx, tt.input)
			require.Error(t, err)
			var appErr *apperror.AppError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, tt.code, appErr.Status)
			assert.Empty(t, repo.notifications)
		})
	}
}

func TestCreateUsesExplicitTenantWhenContextHasNone(t *testing.T) {
	repo := newMockRepository()
	svc := newTestService(repo, &mockEmailSender{})

	result, err := svc.Create(context.Background(), notificationcontracts.CreateNotificationInput{
		TenantID: 2, UserID: 10, Type: "system.info", Title: "Explicit tenant", Body: "Body",
	})
	require.NoError(t, err)
	require.NotNil(t, result.Notification)
	assert.Equal(t, int64(2), result.Notification.TenantID)
}

func TestCreateEmailOnlyWithoutTransactionManager(t *testing.T) {
	repo := newMockRepository()
	sender := &mockEmailSender{}
	recipients := &mockRecipientLookup{recipients: map[int64]identitycontracts.NotificationRecipient{
		10: {UserID: 10, Email: "user10@example.com"},
	}}
	svc := NewNotificationService(repo, recipients, sender, nil)
	ctx := testContext(1)

	_, err := svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{Type: "system.info", Channel: ChannelInApp, Enabled: new(false)})
	require.NoError(t, err)

	result, err := svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "system.info", Title: "Email only", Body: "Body",
	})
	require.NoError(t, err)
	assert.Nil(t, result.Notification)
	assert.True(t, result.EmailQueued)
	assert.Empty(t, repo.notifications)
	require.Len(t, sender.messages, 1)
	assert.Equal(t, 1, sender.sendCalls, "without a transaction manager the plain sender is used")
}

func TestCreateRequiresRecipient(t *testing.T) {
	repo := newMockRepository()
	svc := newTestService(repo, &mockEmailSender{})

	_, err := svc.Create(testContext(1), notificationcontracts.CreateNotificationInput{
		UserID: 99, Type: "system.info", Title: "Title", Body: "Body",
	})
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, appErr.Status)
	assert.Empty(t, repo.notifications)
}

func TestListUnreadReadAndIsolation(t *testing.T) {
	repo := newMockRepository()
	svc := newTestService(repo, &mockEmailSender{})
	ctx := testContext(1)
	now := time.Now().UTC()

	first := seedNotification(repo, 1, 10, "first", false, now.Add(-2*time.Minute))
	second := seedNotification(repo, 1, 10, "second", true, now.Add(-time.Minute))
	seedNotification(repo, 1, 11, "other user", false, now)
	seedNotification(repo, 2, 10, "other tenant", false, now)

	items, hasMore, err := svc.List(ctx, 10, 20, "", false)
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, items, 2)
	assert.Equal(t, second.ID, items[0].ID)
	assert.Equal(t, first.ID, items[1].ID)

	unread, _, err := svc.List(ctx, 10, 20, "", true)
	require.NoError(t, err)
	require.Len(t, unread, 1)
	assert.Equal(t, first.ID, unread[0].ID)

	count, err := svc.UnreadCount(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	read, err := svc.MarkRead(ctx, 10, first.ID)
	require.NoError(t, err)
	require.NotNil(t, read.ReadAt)
	readAgain, err := svc.MarkRead(ctx, 10, first.ID)
	require.NoError(t, err)
	assert.True(t, read.ReadAt.Equal(*readAgain.ReadAt), "mark-read must be idempotent")

	updated, err := svc.MarkAllRead(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(0), updated, "the only remaining unread row belonged to another user")
	count, err = svc.UnreadCount(ctx, 10)
	require.NoError(t, err)
	assert.Zero(t, count)

	_, err = svc.Get(ctx, 10, 3)
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, appErr.Status, "another user's id must not be disclosed")

	_, err = svc.MarkRead(ctx, 10, 4)
	require.Error(t, err)
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, appErr.Status, "another tenant's id must not be disclosed")
}

func TestListCursorPagination(t *testing.T) {
	repo := newMockRepository()
	svc := newTestService(repo, &mockEmailSender{})
	ctx := testContext(1)
	base := time.Now().UTC().Add(-10 * time.Minute)
	seedNotification(repo, 1, 10, "oldest", false, base)
	seedNotification(repo, 1, 10, "middle", false, base.Add(time.Minute))
	seedNotification(repo, 1, 10, "newest", false, base.Add(2*time.Minute))

	page1, hasMore, err := svc.List(ctx, 10, 2, "", false)
	require.NoError(t, err)
	require.True(t, hasMore)
	require.Len(t, page1, 2)
	assert.Equal(t, "newest", page1[0].Title)
	assert.Equal(t, "middle", page1[1].Title)

	cursor := pagination.EncodeCursor(page1[len(page1)-1].CreatedAt, page1[len(page1)-1].ID)
	page2, hasMore, err := svc.List(ctx, 10, 2, cursor, false)
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, page2, 1)
	assert.Equal(t, "oldest", page2[0].Title)

	_, _, err = svc.List(ctx, 10, 2, "not-a-cursor", false)
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 400, appErr.Status)
}

func TestPreferences(t *testing.T) {
	repo := newMockRepository()
	svc := newTestService(repo, &mockEmailSender{})
	ctx := testContext(1)

	prefs, err := svc.ListPreferences(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, prefs)

	pref, err := svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{
		Type: "security.alert", Channel: ChannelEmail, Enabled: new(false),
	})
	require.NoError(t, err)
	assert.False(t, pref.Enabled)
	assert.Equal(t, int64(1), pref.TenantID)
	assert.Equal(t, int64(10), pref.UserID)

	same, err := svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{
		Type: "security.alert", Channel: ChannelEmail, Enabled: new(true),
	})
	require.NoError(t, err)
	assert.True(t, same.Enabled)
	assert.Equal(t, pref.ID, same.ID, "upsert must keep the unique preference row")

	prefs, err = svc.ListPreferences(ctx, 10)
	require.NoError(t, err)
	require.Len(t, prefs, 1)

	for _, req := range []dto.UpdatePreferenceRequest{
		{Type: "security.alert", Channel: "push", Enabled: new(true)},
		{Type: "", Channel: ChannelEmail, Enabled: new(true)},
		{Type: "Security.Alert", Channel: ChannelEmail, Enabled: new(true)},
		// A missing enabled field must not silently disable the channel.
		{Type: "security.alert", Channel: ChannelEmail},
	} {
		_, err := svc.SetPreference(ctx, 10, req)
		require.Error(t, err)
		var appErr *apperror.AppError
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, 400, appErr.Status)
	}
}

func TestCreateRejectsTextPostgresCannotStore(t *testing.T) {
	repo := newMockRepository()
	svc := newTestService(repo, &mockEmailSender{})
	ctx := testContext(1)

	cases := map[string]notificationcontracts.CreateNotificationInput{
		"NUL in title":          {Title: "Hi\x00there", Body: "Body"},
		"NUL in body":           {Title: "Title", Body: "Bo\x00dy"},
		"invalid UTF-8 title":   {Title: "Bad \xff title", Body: "Body"},
		"invalid UTF-8 body":    {Title: "Title", Body: "Bad \xc3\x28 body"},
		"NUL escape in data":    {Title: "Title", Body: "Body", Data: json.RawMessage(`{"k":"a\u0000b"}`)},
		"NUL escape in key":     {Title: "Title", Body: "Body", Data: json.RawMessage(`{"a\u0000":1}`)},
		"nested NUL in data":    {Title: "Title", Body: "Body", Data: json.RawMessage(`{"k":[{"x":"\u0000"}]}`)},
		"invalid UTF-8 in data": {Title: "Title", Body: "Body", Data: json.RawMessage("{\"k\":\"\xff\"}")},
	}
	for name, input := range cases {
		input.UserID, input.Type = 10, "system.info"
		_, err := svc.Create(ctx, input)
		var appErr *apperror.AppError
		require.ErrorAs(t, err, &appErr, name)
		assert.Equal(t, 400, appErr.Status, name)
	}
	assert.Empty(t, repo.notifications, "rejected input must not reach the repository")

	// An escaped backslash followed by "u0000" is ordinary text for jsonb.
	_, err := svc.Create(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "system.info", Title: "Title", Body: "Body",
		Data: json.RawMessage(`{"k":"\\u0000"}`),
	})
	require.NoError(t, err)
}

func TestNotifyHonorsSuppressionAndQueueErrors(t *testing.T) {
	repo := newMockRepository()
	sender := &mockEmailSender{}
	svc := newTestService(repo, sender)
	ctx := testContext(1)

	for _, channel := range []string{ChannelInApp, ChannelEmail} {
		_, err := svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{
			Type: "system.info", Channel: channel, Enabled: new(false),
		})
		require.NoError(t, err)
	}
	err := svc.Notify(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "system.info", Title: "Suppressed", Body: "Body",
	})
	require.NoError(t, err)
	assert.Empty(t, sender.messages)

	_, err = svc.SetPreference(ctx, 10, dto.UpdatePreferenceRequest{
		Type: "system.info", Channel: ChannelEmail, Enabled: new(true),
	})
	require.NoError(t, err)
	sender.fail = errors.New("queue unavailable")
	err = svc.Notify(ctx, notificationcontracts.CreateNotificationInput{
		UserID: 10, Type: "system.info", Title: "Fails", Body: "Body",
	})
	require.Error(t, err)
}
