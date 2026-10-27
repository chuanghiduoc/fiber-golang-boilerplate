package requestctx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestAndActorValues(t *testing.T) {
	ctx := context.Background()
	assert.Empty(t, RequestID(ctx))
	assert.Zero(t, ActorID(ctx))

	ctx = WithRequestID(ctx, "req-1")
	assert.Equal(t, "req-1", RequestID(ctx))
	assert.Zero(t, ActorID(ctx))

	ctx = WithActorID(ctx, testID(42))
	assert.Equal(t, "req-1", RequestID(ctx), "setting the actor must preserve the request id")
	assert.Equal(t, testID(42), ActorID(ctx))

	ctx = WithRequestID(ctx, "req-2")
	assert.Equal(t, "req-2", RequestID(ctx), "setting the request id must preserve the actor")
	assert.Equal(t, testID(42), ActorID(ctx))
}

func TestSessionID(t *testing.T) {
	ctx := context.Background()
	assert.Zero(t, SessionID(ctx))

	sessionID := testID(9)
	ctx = WithSessionID(ctx, sessionID)
	assert.Equal(t, sessionID, SessionID(ctx))

	ctx = WithRequestID(ctx, "req-1")
	assert.Equal(t, sessionID, SessionID(ctx), "setting the request id must preserve the session id")
}
