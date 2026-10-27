package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
)

// Transactor implements service.Transactor: InTx binds the file repository,
// the Tier 1 audit recorder, and the event publisher to one pgx transaction,
// so the service never sees pgx.Tx.
type Transactor struct {
	txManager *database.TxManager
	audit     audit.TxRecorder
	bus       *events.Bus
}

var _ filesvc.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators. A nil bus (unit
// tests, minimal deployments without the event bus) publishes nothing.
func NewTransactor(txManager *database.TxManager, recorder audit.TxRecorder, bus *events.Bus) *Transactor {
	return &Transactor{txManager: txManager, audit: recorder, bus: bus}
}

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx filesvc.TxRepos) error) error {
	return t.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		return fn(ctx, filesvc.TxRepos{
			Files:  NewFileRepository(tx),
			Audit:  txAudit{recorder: t.audit, tx: tx},
			Events: txEvents{bus: t.bus, tx: tx},
		})
	})
}

// txAudit writes Tier 1 audit events through the open transaction. A nil
// recorder fails closed (audit.ErrRecorderUnavailable).
type txAudit struct {
	recorder audit.TxRecorder
	tx       pgx.Tx
}

func (a txAudit) RecordTx(ctx context.Context, event audit.Event) error {
	return audit.RecordInTx(ctx, a.recorder, a.tx, event)
}

// txEvents publishes domain events through the open transaction, so durable
// subscriber jobs (webhook fan-out) commit with the mutation.
type txEvents struct {
	bus *events.Bus
	tx  pgx.Tx
}

func (e txEvents) Publish(ctx context.Context, event events.Event) error {
	if e.bus == nil {
		return nil
	}
	return e.bus.PublishTx(ctx, e.tx, event)
}
