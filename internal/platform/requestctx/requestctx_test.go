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

	ctx = WithActorID(ctx, 42)
	assert.Equal(t, "req-1", RequestID(ctx), "setting the actor must preserve the request id")
	assert.Equal(t, int64(42), ActorID(ctx))

	ctx = WithRequestID(ctx, "req-2")
	assert.Equal(t, "req-2", RequestID(ctx), "setting the request id must preserve the actor")
	assert.Equal(t, int64(42), ActorID(ctx))
}
