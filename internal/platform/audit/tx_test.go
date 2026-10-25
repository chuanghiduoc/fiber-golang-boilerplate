package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedTxRecorder lets one recorder serve both the fallback middleware
// (Record) and a Tier 1 service (RecordInTx), as in cmd/api.
type sharedTxRecorder struct{ recordingRecorder }

func (r *sharedTxRecorder) RecordTx(ctx context.Context, _ pgx.Tx, event Event) error {
	return r.Record(ctx, event)
}

func TestRecordInTx_NilRecorderFailsClosed(t *testing.T) {
	err := RecordInTx(context.Background(), nil, nil, Event{Action: "user.deleted"})
	require.ErrorIs(t, err, ErrRecorderUnavailable)
}

func TestMiddleware_FailedServiceRecordDoesNotMarkRequest(t *testing.T) {
	recorder := &sharedTxRecorder{recordingRecorder{err: errors.New("db down")}}
	info := &RequestInfo{}
	err := RecordInTx(WithRequestInfo(context.Background(), info), recorder, nil, Event{Action: "x"})
	require.Error(t, err)
	assert.Zero(t, info.ServiceRecorded)
}
