package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// mockRepository mirrors the repository's tenant and user predicates. It is
// intentionally in-memory so service tests exercise scoping without Docker.
type mockRepository struct {
	notifications map[uuid.UUID]*Notification
	prefs         map[string]*Preference
	nextNotifID   int
	failCreate    error
	failList      error
}

var _ NotificationRepository = (*mockRepository)(nil)

func newMockRepository() *mockRepository {
	return &mockRepository{
		notifications: make(map[uuid.UUID]*Notification),
		prefs:         make(map[string]*Preference),
		nextNotifID:   1,
	}
}

func (m *mockRepository) Create(ctx context.Context, in NewNotification) (*Notification, error) {
	if m.failCreate != nil {
		return nil, m.failCreate
	}
	data := map[string]any{}
	if len(in.Data) > 0 {
		_ = json.Unmarshal(in.Data, &data)
	}
	row := &Notification{
		ID:        testID(m.nextNotifID),
		TenantID:  tenant.IDFromContext(ctx),
		UserID:    in.UserID,
		Type:      in.Type,
		Title:     in.Title,
		Body:      in.Body,
		Data:      data,
		CreatedAt: time.Now().UTC(),
	}
	m.notifications[row.ID] = row
	m.nextNotifID++
	return row, nil
}

func (m *mockRepository) GetByID(ctx context.Context, id, userID uuid.UUID) (*Notification, error) {
	row, ok := m.notifications[id]
	if !ok || !m.scoped(ctx, row, userID) {
		return nil, apperror.ErrNotFound
	}
	return row, nil
}

func (m *mockRepository) ListCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor, unreadOnly bool) ([]Notification, error) {
	if m.failList != nil {
		return nil, m.failList
	}
	rows := make([]Notification, 0, len(m.notifications))
	for _, row := range m.notifications {
		if !m.scoped(ctx, row, userID) {
			continue
		}
		if unreadOnly && row.ReadAt != nil {
			continue
		}
		if cur.HasCursor && !beforeCursor(row, cur) {
			continue
		}
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return bytes.Compare(rows[i].ID[:], rows[j].ID[:]) > 0
		}
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	if len(rows) > int(cur.Limit) {
		rows = rows[:cur.Limit]
	}
	return rows, nil
}

func (m *mockRepository) CountUnread(ctx context.Context, userID uuid.UUID) (int64, error) {
	var count int64
	for _, row := range m.notifications {
		if m.scoped(ctx, row, userID) && row.ReadAt == nil {
			count++
		}
	}
	return count, nil
}

func (m *mockRepository) MarkRead(ctx context.Context, id, userID uuid.UUID) (*Notification, error) {
	row, ok := m.notifications[id]
	if !ok || !m.scoped(ctx, row, userID) {
		return nil, apperror.ErrNotFound
	}
	if row.ReadAt == nil {
		readAt := time.Now().UTC()
		row.ReadAt = &readAt
	}
	return row, nil
}

func (m *mockRepository) MarkUnread(ctx context.Context, id, userID uuid.UUID) (*Notification, error) {
	row, ok := m.notifications[id]
	if !ok || !m.scoped(ctx, row, userID) {
		return nil, apperror.ErrNotFound
	}
	row.ReadAt = nil
	return row, nil
}

func (m *mockRepository) MarkAllRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	var updated int64
	for _, row := range m.notifications {
		if m.scoped(ctx, row, userID) && row.ReadAt == nil {
			readAt := time.Now().UTC()
			row.ReadAt = &readAt
			updated++
		}
	}
	return updated, nil
}

func (m *mockRepository) GetPreference(ctx context.Context, userID uuid.UUID, notificationType, channel string) (*Preference, error) {
	row, ok := m.prefs[prefKey(tenant.IDFromContext(ctx), userID, notificationType, channel)]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return row, nil
}

func (m *mockRepository) ListPreferences(ctx context.Context, userID uuid.UUID) ([]Preference, error) {
	rows := make([]Preference, 0, len(m.prefs))
	for _, row := range m.prefs {
		if row.TenantID == tenant.IDFromContext(ctx) && row.UserID == userID {
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

func (m *mockRepository) UpsertPreference(ctx context.Context, in NewPreference) (*Preference, error) {
	tenantID := tenant.IDFromContext(ctx)
	key := prefKey(tenantID, in.UserID, in.Type, in.Channel)
	if row, ok := m.prefs[key]; ok {
		row.Enabled = in.Enabled
		row.UpdatedAt = time.Now().UTC()
		return row, nil
	}
	now := time.Now().UTC()
	row := &Preference{
		TenantID:  tenantID,
		UserID:    in.UserID,
		Type:      in.Type,
		Channel:   in.Channel,
		Enabled:   in.Enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.prefs[key] = row
	return row, nil
}

func (m *mockRepository) scoped(ctx context.Context, row *Notification, userID uuid.UUID) bool {
	return row.TenantID == tenant.IDFromContext(ctx) && row.UserID == userID
}

func (m *mockRepository) snapshot() (notifs map[uuid.UUID]Notification, prefs map[string]Preference, nextNotifID int) {
	notifs = make(map[uuid.UUID]Notification, len(m.notifications))
	for id, row := range m.notifications {
		notifs[id] = *row
	}
	prefs = make(map[string]Preference, len(m.prefs))
	for key, row := range m.prefs {
		prefs[key] = *row
	}
	return notifs, prefs, m.nextNotifID
}

func (m *mockRepository) restore(notifs map[uuid.UUID]Notification, prefs map[string]Preference, nextNotifID int) {
	m.notifications = make(map[uuid.UUID]*Notification, len(notifs))
	m.nextNotifID = nextNotifID
	for id, row := range notifs {
		copyRow := row
		m.notifications[id] = &copyRow
	}
	m.prefs = make(map[string]*Preference, len(prefs))
	for key, row := range prefs {
		copyRow := row
		m.prefs[key] = &copyRow
	}
}

func beforeCursor(row *Notification, cur pagination.DBCursor) bool {
	if row.CreatedAt.Before(cur.CreatedAt) {
		return true
	}
	return row.CreatedAt.Equal(cur.CreatedAt) && bytes.Compare(row.ID[:], cur.ID[:]) < 0
}

func prefKey(tenantID, userID uuid.UUID, notificationType, channel string) string {
	return fmt.Sprintf("%s|%s|%s|%s", tenantID, userID, notificationType, channel)
}

// mockTransactor models transaction rollback without a database.
type mockTransactor struct {
	repo *mockRepository
	mail *mockEmailSender
}

var _ Transactor = (*mockTransactor)(nil)

func (m *mockTransactor) InTx(ctx context.Context, fn func(context.Context, TxRepos) error) error {
	notifs, prefs, nextNotifID := m.repo.snapshot()
	if err := fn(ctx, TxRepos{Notifications: m.repo, Mail: txMail{sender: m.mail}}); err != nil {
		m.repo.restore(notifs, prefs, nextNotifID)
		return err
	}
	return nil
}

type txMail struct {
	sender *mockEmailSender
}

func (m txMail) Send(ctx context.Context, msg email.Message) error {
	m.sender.txCalls++
	return m.sender.Send(ctx, msg)
}

type mockEmailSender struct {
	messages  []email.Message
	fail      error
	sendCalls int
	txCalls   int
}

var _ Mailer = (*mockEmailSender)(nil)

func (m *mockEmailSender) Send(_ context.Context, msg email.Message) error {
	m.sendCalls++
	if m.fail != nil {
		return m.fail
	}
	m.messages = append(m.messages, msg)
	return nil
}

type mockRecipientLookup struct {
	recipients map[uuid.UUID]identitycontracts.NotificationRecipient
	err        error
}

func (m *mockRecipientLookup) NotificationRecipient(_ context.Context, userID uuid.UUID) (identitycontracts.NotificationRecipient, error) {
	if m.err != nil {
		return identitycontracts.NotificationRecipient{}, m.err
	}
	recipient, ok := m.recipients[userID]
	if !ok {
		return identitycontracts.NotificationRecipient{}, apperror.ErrNotFound
	}
	return recipient, nil
}
