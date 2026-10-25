package privacy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	notifications []Notification
	preferences   []Preference
	deleted       bool
}

func (f *fakeStore) ListNotifications(context.Context, int64, int64) ([]Notification, error) {
	return f.notifications, nil
}
func (f *fakeStore) ListPreferences(context.Context, int64, int64) ([]Preference, error) {
	return f.preferences, nil
}
func (f *fakeStore) DeleteAll(context.Context, int64, int64) error {
	f.deleted = true
	return nil
}

func TestExportIncludesFeedAndPreferences(t *testing.T) {
	store := &fakeStore{
		notifications: []Notification{{ID: 1, Type: "security.alert", Title: "Hi", Body: "Body", Data: json.RawMessage(`{"k":"v"}`), CreatedAt: time.Now()}},
		preferences:   []Preference{{Type: "security.alert", Channel: "email", Enabled: false}},
	}
	section, err := New(store).Export(context.Background(), 1, 10)
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
	require.NoError(t, New(store).Erase(context.Background(), 1, 10))
	assert.True(t, store.deleted)
}
