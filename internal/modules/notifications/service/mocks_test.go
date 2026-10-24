package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// mockRepository mirrors the repository's tenant and user predicates. It is
// intentionally in-memory so service tests exercise scoping without Docker.
type mockRepository struct {
	notifications map[int64]*notificationsqlc.Notification
	prefs         map[string]*notificationsqlc.NotificationPreference
	nextNotifID   int64
	nextPrefID    int64
	failCreate    error
	failList      error
}

var _ repository.NotificationRepository = (*mockRepository)(nil)

func newMockRepository() *mockRepository {
	return &mockRepository{
		notifications: make(map[int64]*notificationsqlc.Notification),
		prefs:         make(map[string]*notificationsqlc.NotificationPreference),
		nextNotifID:   1,
		nextPrefID:    1,
	}
}

func (m *mockRepository) WithTx(tx pgx.Tx) repository.NotificationRepository { return m }

func (m *mockRepository) Create(ctx context.Context, params notificationsqlc.CreateNotificationParams) (*notificationsqlc.Notification, error) {
	if m.failCreate != nil {
		return nil, m.failCreate
	}
	params.TenantID = tenant.EffectiveID(ctx)
	row := &notificationsqlc.Notification{
		ID:        m.nextNotifID,
		TenantID:  params.TenantID,
		UserID:    params.UserID,
		Type:      params.Type,
		Title:     params.Title,
		Body:      params.Body,
		Data:      append([]byte(nil), params.Data...),
		CreatedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}
	m.notifications[row.ID] = row
	m.nextNotifID++
	return row, nil
}

func (m *mockRepository) GetByID(ctx context.Context, id, userID int64) (*notificationsqlc.Notification, error) {
	row, ok := m.notifications[id]
	if !ok || !m.scoped(ctx, row, userID) {
		return nil, apperror.ErrNotFound
	}
	return row, nil
}

func (m *mockRepository) ListCursor(ctx context.Context, userID int64, cur pagination.DBCursor, unreadOnly bool) ([]notificationsqlc.Notification, error) {
	if m.failList != nil {
		return nil, m.failList
	}
	rows := make([]notificationsqlc.Notification, 0, len(m.notifications))
	for _, row := range m.notifications {
		if !m.scoped(ctx, row, userID) {
			continue
		}
		if unreadOnly && row.ReadAt.Valid {
			continue
		}
		if cur.HasCursor && !beforeCursor(row, cur) {
			continue
		}
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Time.Equal(rows[j].CreatedAt.Time) {
			return rows[i].ID > rows[j].ID
		}
		return rows[i].CreatedAt.Time.After(rows[j].CreatedAt.Time)
	})
	if len(rows) > int(cur.Limit) {
		rows = rows[:cur.Limit]
	}
	return rows, nil
}

func (m *mockRepository) CountUnread(ctx context.Context, userID int64) (int64, error) {
	var count int64
	for _, row := range m.notifications {
		if m.scoped(ctx, row, userID) && !row.ReadAt.Valid {
			count++
		}
	}
	return count, nil
}

func (m *mockRepository) MarkRead(ctx context.Context, id, userID int64) (*notificationsqlc.Notification, error) {
	row, ok := m.notifications[id]
	if !ok || !m.scoped(ctx, row, userID) {
		return nil, apperror.ErrNotFound
	}
	if !row.ReadAt.Valid {
		row.ReadAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	return row, nil
}

func (m *mockRepository) MarkAllRead(ctx context.Context, userID int64) (int64, error) {
	var updated int64
	for _, row := range m.notifications {
		if m.scoped(ctx, row, userID) && !row.ReadAt.Valid {
			row.ReadAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
			updated++
		}
	}
	return updated, nil
}

func (m *mockRepository) GetPreference(ctx context.Context, userID int64, notificationType, channel string) (*notificationsqlc.NotificationPreference, error) {
	row, ok := m.prefs[prefKey(tenant.EffectiveID(ctx), userID, notificationType, channel)]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return row, nil
}

func (m *mockRepository) ListPreferences(ctx context.Context, userID int64) ([]notificationsqlc.NotificationPreference, error) {
	rows := make([]notificationsqlc.NotificationPreference, 0, len(m.prefs))
	for _, row := range m.prefs {
		if row.TenantID == tenant.EffectiveID(ctx) && row.UserID == userID {
			rows = append(rows, *row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type == rows[j].Type {
			return rows[i].Channel < rows[j].Channel
		}
		return rows[i].Type < rows[j].Type
	})
	return rows, nil
}

func (m *mockRepository) UpsertPreference(ctx context.Context, params notificationsqlc.UpsertNotificationPreferenceParams) (*notificationsqlc.NotificationPreference, error) {
	params.TenantID = tenant.EffectiveID(ctx)
	key := prefKey(params.TenantID, params.UserID, params.Type, params.Channel)
	if row, ok := m.prefs[key]; ok {
		row.Enabled = params.Enabled
		row.UpdatedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
		return row, nil
	}
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	row := &notificationsqlc.NotificationPreference{
		ID:        m.nextPrefID,
		TenantID:  params.TenantID,
		UserID:    params.UserID,
		Type:      params.Type,
		Channel:   params.Channel,
		Enabled:   params.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.prefs[key] = row
	m.nextPrefID++
	return row, nil
}

func (m *mockRepository) scoped(ctx context.Context, row *notificationsqlc.Notification, userID int64) bool {
	return row.TenantID == tenant.EffectiveID(ctx) && row.UserID == userID
}

func (m *mockRepository) snapshot() (notifs map[int64]notificationsqlc.Notification, prefs map[string]notificationsqlc.NotificationPreference) {
	notifs = make(map[int64]notificationsqlc.Notification, len(m.notifications))
	for id, row := range m.notifications {
		notifs[id] = *row
	}
	prefs = make(map[string]notificationsqlc.NotificationPreference, len(m.prefs))
	for key, row := range m.prefs {
		prefs[key] = *row
	}
	return notifs, prefs
}

func (m *mockRepository) restore(notifs map[int64]notificationsqlc.Notification, prefs map[string]notificationsqlc.NotificationPreference) {
	m.notifications = make(map[int64]*notificationsqlc.Notification, len(notifs))
	m.nextNotifID = 1
	for id, row := range notifs {
		copyRow := row
		m.notifications[id] = &copyRow
		if id >= m.nextNotifID {
			m.nextNotifID = id + 1
		}
	}
	m.prefs = make(map[string]*notificationsqlc.NotificationPreference, len(prefs))
	m.nextPrefID = 1
	for key, row := range prefs {
		copyRow := row
		m.prefs[key] = &copyRow
		if row.ID >= m.nextPrefID {
			m.nextPrefID = row.ID + 1
		}
	}
}

func beforeCursor(row *notificationsqlc.Notification, cur pagination.DBCursor) bool {
	if row.CreatedAt.Time.Before(cur.CreatedAt) {
		return true
	}
	return row.CreatedAt.Time.Equal(cur.CreatedAt) && row.ID < cur.ID
}

func prefKey(tenantID, userID int64, notificationType, channel string) string {
	return fmt.Sprintf("%d|%d|%s|%s", tenantID, userID, notificationType, channel)
}

// mockTxManager models transaction rollback without a database.
type mockTxManager struct {
	repo *mockRepository
}

func (m *mockTxManager) WithTx(_ context.Context, fn func(tx pgx.Tx) error) error {
	notifs, prefs := m.repo.snapshot()
	if err := fn(nil); err != nil {
		m.repo.restore(notifs, prefs)
		return err
	}
	return nil
}

type mockEmailSender struct {
	messages  []email.Message
	fail      error
	sendCalls int
	txCalls   int
}

func (m *mockEmailSender) Send(_ context.Context, msg email.Message) error {
	m.sendCalls++
	if m.fail != nil {
		return m.fail
	}
	m.messages = append(m.messages, msg)
	return nil
}

func (m *mockEmailSender) SendTx(_ context.Context, _ pgx.Tx, msg email.Message) error {
	m.txCalls++
	if m.fail != nil {
		return m.fail
	}
	m.messages = append(m.messages, msg)
	return nil
}

type mockRecipientLookup struct {
	recipients map[int64]identitycontracts.NotificationRecipient
	err        error
}

func (m *mockRecipientLookup) NotificationRecipient(_ context.Context, userID int64) (identitycontracts.NotificationRecipient, error) {
	if m.err != nil {
		return identitycontracts.NotificationRecipient{}, m.err
	}
	recipient, ok := m.recipients[userID]
	if !ok {
		return identitycontracts.NotificationRecipient{}, apperror.ErrNotFound
	}
	return recipient, nil
}
