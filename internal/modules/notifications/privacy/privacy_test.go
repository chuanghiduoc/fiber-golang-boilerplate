package privacy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	truncated     bool
}

func (f *fakeStore) ListNotifications(context.Context, uuid.UUID, uuid.UUID, int32) ([]Notification, bool, error) {
	return f.notifications, f.truncated, nil
}
func (f *fakeStore) ListPreferences(context.Context, uuid.UUID, uuid.UUID, int32) ([]Preference, bool, error) {
	return f.preferences, f.truncated, nil
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

func TestExportMarksStoreTruncation(t *testing.T) {
	store := &fakeStore{
		notifications: []Notification{{ID: testID(1), Type: "security.alert", Title: "Hi", Body: "Body", CreatedAt: time.Now()}},
		truncated:     true,
	}
	section, err := New(store).Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	var out struct {
		Budget struct {
			Truncated bool `json:"truncated"`
			Returned  int  `json:"returned"`
		} `json:"budget"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	assert.True(t, section.Truncated)
	assert.True(t, out.Budget.Truncated)
	assert.Equal(t, 1, out.Budget.Returned)
}

func TestExportStopsAtSectionByteBudget(t *testing.T) {
	store := &fakeStore{notifications: []Notification{
		{ID: testID(1), Type: "security.alert", Title: "One", Body: strings.Repeat("a", 200), CreatedAt: time.Now()},
		{ID: testID(2), Type: "security.alert", Title: "Two", Body: strings.Repeat("b", 200), CreatedAt: time.Now()},
	}}
	provider := New(store)
	provider.byteLimit = 1500
	section, err := provider.Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	var out struct {
		Notifications []struct {
			ID string `json:"id"`
		} `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.Notifications, 1)
	assert.True(t, section.Truncated)
}
