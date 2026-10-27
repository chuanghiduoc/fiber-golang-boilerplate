package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

type fakeStore struct {
	notifications []Notification
	preferences   []Preference
	deleted       bool
}

func (f *fakeStore) ListNotifications(context.Context, uuid.UUID, uuid.UUID) ([]Notification, error) {
	return f.notifications, nil
}
func (f *fakeStore) ListPreferences(context.Context, uuid.UUID, uuid.UUID) ([]Preference, error) {
	return f.preferences, nil
}
func (f *fakeStore) DeleteAll(context.Context, uuid.UUID, uuid.UUID) error {
	f.deleted = true
	return nil
}

func TestExportIncludesFeedAndPreferences(t *testing.T) {
	store := &fakeStore{
		notifications: []Notification{{ID: testID(1), Type: "security.alert", Title: "Hi", Body: "Body", Data: json.RawMessage(`{"k":"v"}`), CreatedAt: time.Now()}},
		preferences:   []Preference{{Type: "security.alert", Channel: "email", Enabled: false}},
	}
	section, err := New(store).Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	assert.Equal(t, "notifications", section.Name)
	var out struct {
		Notifications []struct {
			Type string `json:"type"`
		} `json:"notifications"`
		Preferences []struct {
			Channel string `json:"channel"`
			Enabled bool   `json:"enabled"`
		} `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.Notifications, 1)
	assert.Equal(t, "security.alert", out.Notifications[0].Type)
	require.Len(t, out.Preferences, 1)
	assert.Equal(t, "email", out.Preferences[0].Channel)
	assert.False(t, out.Preferences[0].Enabled)
}

func TestEraseDeletesFeedAndPreferences(t *testing.T) {
	store := &fakeStore{}
	require.NoError(t, New(store).Erase(context.Background(), testID(1), testID(10)))
	assert.True(t, store.deleted)
}
