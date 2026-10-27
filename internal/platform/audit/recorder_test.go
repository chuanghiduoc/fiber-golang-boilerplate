package audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type recordingRecorder struct {
	events []Event
	err    error
}

func (r *recordingRecorder) Record(_ context.Context, event Event) error {
	r.events = append(r.events, event)
	return r.err
}

func TestNoopIsNotAnIntentRecorder(t *testing.T) {
	var recorder any = Noop{}
	_, implementsID := recorder.(IDRecorder)
	_, implementsCompletion := recorder.(CompletionRecorder)
	assert.False(t, implementsID)
	assert.False(t, implementsCompletion)
}

func TestNewServiceEvent_UsesRequestIdentity(t *testing.T) {
	info := &RequestInfo{
		EventID:            testID(9),
		TenantID:           testID(3),
		ActorID:            testID(42),
		ActorType:          ActorTypeUser,
		ActorRole:          "admin",
		RequestID:          "req-1",
		IP:                 "203.0.113.9",
		UserAgent:          "test",
		IdempotencyKeyHash: []byte("hash"),
	}
	ctx := WithRequestInfo(context.Background(), info)
	event := NewServiceEvent(ctx, "user.banned", "user")

	assert.Equal(t, testID(3), event.TenantID)
	assert.Equal(t, testID(9), event.ParentID)
	assert.Equal(t, testID(42), event.ActorID)
	assert.Equal(t, ActorTypeUser, event.ActorType)
	assert.Equal(t, SourceService, event.Source)
	assert.Equal(t, OutcomeSucceeded, event.Outcome)
	assert.Equal(t, "req-1", event.RequestID)
	assert.Equal(t, []byte("hash"), event.IdempotencyKeyHash)
	assert.Equal(t, 1, info.DomainEvents)
	assert.WithinDuration(t, time.Now(), event.CreatedAt, time.Minute)
}

// testID returns a deterministic UUID for a small fixture number.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}
