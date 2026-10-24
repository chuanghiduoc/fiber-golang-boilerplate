package audit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// RecordInTx writes a terminal event through tx when one is supplied. It is the
// bridge used by Tier 1 services that may run with or without a transaction
// manager in tests.
//
// Production services always have a transaction manager. A nil transaction
// falls back to the pool recorder and increments audit_tx_missing_total, which
// operators should alert on: the mutation and its audit row are no longer
// atomic.
func RecordInTx(ctx context.Context, recorder TxRecorder, tx pgx.Tx, event Event) error {
	if recorder == nil {
		return nil
	}
	if tx != nil {
		return recorder.RecordTx(ctx, tx, event)
	}
	metrics.AuditTxMissingTotal.Inc()
	return recorder.Record(ctx, event)
}
