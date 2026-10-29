package privacy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

type fakeStore struct {
	events        []Event
	deliveries    []Delivery
	deleted       bool
	eventCalls    int
	deliveryCalls int
}

func (f *fakeStore) ListEvents(_ context.Context, _, _ uuid.UUID, cursor pagination.DBCursor) ([]Event, error) {
	f.eventCalls++
	return pageEvents(f.events, cursor), nil
}
func (f *fakeStore) ListDeliveries(_ context.Context, _, _ uuid.UUID, cursor pagination.DBCursor) ([]Delivery, error) {
	f.deliveryCalls++
	return pageDeliveries(f.deliveries, cursor), nil
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

func TestExportBoundsEachCollectionAndReportsTruncation(t *testing.T) {
	limit := int(privacycontracts.DefaultRowLimit)
	events := make([]Event, limit+1)
	deliveries := make([]Delivery, limit+1)
	for i := range events {
		events[i] = Event{
			ID: testID(i + 1), Type: "test.event", DataVersion: 1,
			OccurredAt: time.Unix(int64(len(events)-i), 0),
		}
		deliveries[i] = Delivery{
			ID: testID(i + 1), EventID: testID(i + 1), URL: "https://example.test/hook",
			Status: "succeeded", CreatedAt: time.Unix(int64(len(deliveries)-i), 0),
		}
	}
	store := &fakeStore{events: events, deliveries: deliveries}

	section, err := New(store).Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	var out struct {
		Events     []json.RawMessage `json:"events"`
		Deliveries []json.RawMessage `json:"deliveries"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	assert.Len(t, out.Events, limit)
	assert.Len(t, out.Deliveries, limit)
	assert.True(t, section.Truncated)
	assert.Equal(t, 1, store.eventCalls)
	assert.Equal(t, 1, store.deliveryCalls)
}

func TestExportStopsAtByteBudget(t *testing.T) {
	payload := json.RawMessage(`{"data":"` + strings.Repeat("x", 500) + `"}`)
	events := make([]Event, 4)
	for i := range events {
		events[i] = Event{
			ID: testID(i + 1), Type: "test.event", DataVersion: 1,
			OccurredAt: time.Unix(int64(len(events)-i), 0), Payload: payload,
		}
	}
	store := &fakeStore{events: events}
	provider := New(store)
	provider.rowLimit = 10
	provider.byteLimit = 4096

	section, err := provider.Export(context.Background(), testID(1), testID(10))
	require.NoError(t, err)
	var out struct {
		Events []json.RawMessage `json:"events"`
		Budget struct {
			Returned  int  `json:"returned"`
			Truncated bool `json:"truncated"`
		} `json:"budget"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &out))
	assert.Less(t, out.Budget.Returned, len(events))
	assert.True(t, out.Budget.Truncated)
	assert.True(t, section.Truncated)
	assert.LessOrEqual(t, len(section.Data), int(provider.byteLimit))
}
func pageEvents(events []Event, cursor pagination.DBCursor) []Event {
	start := pageStart(len(events), func(i int) uuid.UUID { return events[i].ID }, cursor)
	limit := pageLimit(cursor)
	end := min(start+limit, len(events))
	return events[start:end]
}

func pageDeliveries(deliveries []Delivery, cursor pagination.DBCursor) []Delivery {
	start := pageStart(len(deliveries), func(i int) uuid.UUID { return deliveries[i].ID }, cursor)
	limit := pageLimit(cursor)
	end := min(start+limit, len(deliveries))
	return deliveries[start:end]
}

func pageStart(length int, idAt func(int) uuid.UUID, cursor pagination.DBCursor) int {
	if !cursor.HasCursor {
		return 0
	}
	for i := 0; i < length; i++ {
		if idAt(i) == cursor.ID {
			return i + 1
		}
	}
	return length
}

func pageLimit(cursor pagination.DBCursor) int {
	if cursor.Limit <= 0 {
		return pagination.DefaultLimit
	}
	return int(cursor.Limit)
}
