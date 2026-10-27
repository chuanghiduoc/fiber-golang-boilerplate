package notifications

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
)

type recordingNotifier struct {
	got []notificationcontracts.CreateNotificationInput
}

func (r *recordingNotifier) Notify(_ context.Context, in notificationcontracts.CreateNotificationInput) error {
	r.got = append(r.got, in)
	return nil
}

// TestDataExportReadyEventCreatesNotification proves the durable subscription
// replaces the old identity→notifications late binding: identity only
// publishes a domain event, and this handler builds the in-app notification.
func TestDataExportReadyEventCreatesNotification(t *testing.T) {
	notifier := &recordingNotifier{}
	m := &Module{notifier: notifier}
	exportID := uuid.MustParse("0192c9b0-7e4a-7000-8000-0000000000aa")
	userID := uuid.MustParse("0192c9b0-7e4a-7000-8000-0000000000bb")
	data, err := json.Marshal(identitycontracts.DataExportReady{ExportID: exportID, UserID: userID})
	require.NoError(t, err)

	event := events.Event{
		Type:     identitycontracts.EventDataExportReady,
		TenantID: uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001"),
		Actor:    events.UserActor(userID),
		Data:     data,
	}
	require.NoError(t, m.handleDataExportReady(context.Background(), event))

	require.Len(t, notifier.got, 1)
	got := notifier.got[0]
	assert.Equal(t, event.TenantID, got.TenantID)
	assert.Equal(t, userID, got.UserID)
	assert.Equal(t, "privacy.data_export_ready", got.Type)
	var payload map[string]string
	require.NoError(t, json.Unmarshal(got.Data, &payload))
	assert.Equal(t, exportID.String(), payload["exportId"])
}

// TestDataExportReadyEventRejectsMalformedPayload keeps an undecodable event
// from retrying forever.
func TestDataExportReadyEventRejectsMalformedPayload(t *testing.T) {
	m := &Module{notifier: &recordingNotifier{}}
	err := m.handleDataExportReady(context.Background(), events.Event{Data: json.RawMessage(`{`)})
	require.Error(t, err)
}
