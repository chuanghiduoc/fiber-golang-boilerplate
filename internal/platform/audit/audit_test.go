package audit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopRecord(t *testing.T) {
	t.Parallel()

	var recorder Recorder = Noop{}
	require.NoError(t, recorder.Record(context.Background(), Event{
		ActorID:  1,
		Action:   "user.login",
		Resource: "user",
	}))
}

func TestEventValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		event   Event
		wantErr bool
	}{
		{
			name:  "valid",
			event: Event{Action: "user.login", Resource: "user"},
		},
		{
			name:    "missing action",
			event:   Event{Resource: "user"},
			wantErr: true,
		},
		{
			name:    "blank action",
			event:   Event{Action: "   ", Resource: "user"},
			wantErr: true,
		},
		{
			name:    "missing resource",
			event:   Event{Action: "user.login"},
			wantErr: true,
		},
		{
			name:    "blank resource",
			event:   Event{Action: "user.login", Resource: "\t"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.event.Validate()
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidEvent), "error should wrap ErrInvalidEvent: %v", err)
		})
	}
}

func TestPostgresRecorderRejectsInvalidEventBeforeTouchingPool(t *testing.T) {
	t.Parallel()

	// A nil pool proves validation runs before any database call.
	recorder := NewPostgresRecorder(nil)

	err := recorder.Record(context.Background(), Event{Resource: "user"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEvent)

	err = recorder.Record(context.Background(), Event{Action: "user.login"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidEvent)
}

func TestPostgresRecorderRejectsUnmarshalableMetadataBeforeTouchingPool(t *testing.T) {
	t.Parallel()

	recorder := NewPostgresRecorder(nil)

	err := recorder.Record(context.Background(), Event{
		Action:   "user.login",
		Resource: "user",
		Metadata: map[string]any{"bad": make(chan int)},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal metadata")
}

func TestMarshalMetadata(t *testing.T) {
	t.Parallel()

	t.Run("nil map becomes empty object", func(t *testing.T) {
		t.Parallel()

		encoded, err := MarshalMetadata(nil)
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(encoded))
	})

	t.Run("empty map becomes empty object", func(t *testing.T) {
		t.Parallel()

		encoded, err := MarshalMetadata(map[string]any{})
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(encoded))
	})

	t.Run("values are preserved", func(t *testing.T) {
		t.Parallel()

		encoded, err := MarshalMetadata(map[string]any{
			"email":  "user@example.com",
			"admin":  true,
			"count":  float64(3),
			"labels": []string{"a", "b"},
		})
		require.NoError(t, err)

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		assert.Equal(t, "user@example.com", decoded["email"])
		assert.Equal(t, true, decoded["admin"])
		assert.Equal(t, float64(3), decoded["count"])
		assert.Equal(t, []any{"a", "b"}, decoded["labels"])
	})
}

func TestNullableHelpers(t *testing.T) {
	t.Parallel()

	assert.Nil(t, nullableInt64(0))
	require.NotNil(t, nullableInt64(7))
	assert.Equal(t, int64(7), *nullableInt64(7))

	assert.Nil(t, nullableString(""))
	require.NotNil(t, nullableString("1.2.3.4"))
	assert.Equal(t, "1.2.3.4", *nullableString("1.2.3.4"))
}
