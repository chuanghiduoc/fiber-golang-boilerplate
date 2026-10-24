package audit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// ErrRecorderUnavailable is returned by RecordInTx when no recorder is
// configured. Tier 1 audit is fail-closed: callers map any RecordInTx error to
// 503 system.audit_unavailable, so a missing recorder must never let the
// mutation succeed unaudited. Tests that do not exercise audit pass Noop{}.
var ErrRecorderUnavailable = errors.New("audit: recorder is not configured")

// RecordInTx writes a terminal event through tx when one is supplied. It is the
// bridge used by Tier 1 services that may run with or without a transaction
// manager in tests.
//
// Production services always have a transaction manager. A nil transaction
// falls back to the pool recorder and increments audit_tx_missing_total, which
// operators should alert on: the mutation and its audit row are no longer
// atomic.
//
// A successful write marks the request's RequestInfo (when present) so the
// fallback Middleware does not record the same mutation a second time.
func RecordInTx(ctx context.Context, recorder TxRecorder, tx pgx.Tx, event Event) error {
	if recorder == nil {
		metrics.AuditWriteFailuresTotal.Inc()
		return ErrRecorderUnavailable
	}
	var err error
	if tx != nil {
		err = recorder.RecordTx(ctx, tx, event)
	} else {
		metrics.AuditTxMissingTotal.Inc()
		err = recorder.Record(ctx, event)
	}
	if err != nil {
		return err
	}
	if info := RequestInfoFromContext(ctx); info != nil {
		info.ServiceRecorded++
	}
	return nil
}
