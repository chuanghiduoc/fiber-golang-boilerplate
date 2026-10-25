package audit

import (
	"context"
	"testing"
	"time"

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
		EventID:            9,
		TenantID:           3,
		ActorID:            42,
		ActorType:          ActorTypeUser,
		ActorRole:          "admin",
		RequestID:          "req-1",
		IP:                 "203.0.113.9",
		UserAgent:          "test",
		IdempotencyKeyHash: "hash",
	}
	ctx := WithRequestInfo(context.Background(), info)
	event := NewServiceEvent(ctx, "user.banned", "user")

	assert.Equal(t, int64(3), event.TenantID)
	assert.Equal(t, int64(9), event.ParentID)
	assert.Equal(t, int64(42), event.ActorID)
	assert.Equal(t, ActorTypeUser, event.ActorType)
	assert.Equal(t, SourceService, event.Source)
	assert.Equal(t, OutcomeSucceeded, event.Outcome)
	assert.Equal(t, "req-1", event.RequestID)
	assert.Equal(t, "hash", event.IdempotencyKeyHash)
	assert.Equal(t, 1, info.DomainEvents)
	assert.WithinDuration(t, time.Now(), event.CreatedAt, time.Minute)
}
