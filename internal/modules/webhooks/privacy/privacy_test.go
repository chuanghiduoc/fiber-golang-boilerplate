package privacy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	events     []Event
	deliveries []Delivery
	deleted    bool
}

func (f *fakeStore) ListEvents(context.Context, uuid.UUID, uuid.UUID) ([]Event, error) {
	return f.events, nil
}
func (f *fakeStore) ListDeliveries(context.Context, uuid.UUID, uuid.UUID) ([]Delivery, error) {
	return f.deliveries, nil
}
func (f *fakeStore) DeleteEvents(context.Context, uuid.UUID, uuid.UUID) error {
	f.deleted = true
	return nil
}

func TestExportIncludesAttributedEventsAndDeliveries(t *testing.T) {
	store := &fakeStore{
		events: []Event{{
			ID: testID(1), Type: "user.registered", DataVersion: 1,
			ActorType: "user", ActorID: "10", OccurredAt: time.Now(),
			Payload: json.RawMessage(`{"userId":10,"email":"a@example.com"}`),
		}},
		deliveries: []Delivery{{ID: testID(2), EventID: testID(1), URL: "https://example.test/hook", Status: "succeeded", Attempts: 1}},
	}
	section, err := New(store).Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	assert.Equal(t, "webhooks", section.Name)
	var out struct {
		Events []struct {
			Type    string `json:"type"`
			Payload struct {
				Email string `json:"email"`
			} `json:"payload"`
		} `json:"events"`
		Deliveries []struct {
			EventID uuid.UUID `json:"eventId"`
		} `json:"deliveries"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	require.Len(t, out.Events, 1)
	assert.Equal(t, "user.registered", out.Events[0].Type)
	assert.Equal(t, "a@example.com", out.Events[0].Payload.Email)
	require.Len(t, out.Deliveries, 1)
	assert.Equal(t, testID(1), out.Deliveries[0].EventID)
}

func TestEraseDeletesAttributedEvents(t *testing.T) {
	store := &fakeStore{}
	require.NoError(t, New(store).Erase(context.Background(), testID(1), testID(10)))
	assert.True(t, store.deleted)
}
