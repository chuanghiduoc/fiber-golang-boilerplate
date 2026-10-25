package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// WebhookTxDispatcher is the transactional side of the webhooks dispatcher. It
// cannot live in the stdlib-only contracts package because it takes the
// producer's pgx.Tx.
type WebhookTxDispatcher interface {
	DispatchTx(ctx context.Context, tx pgx.Tx, events ...webhookcontracts.Event) error
}

// Transactor implements service.Transactor: InTx binds the file repository,
// the Tier 1 audit recorder, and the webhook outbox to one pgx transaction,
// so the service never sees pgx.Tx.
type Transactor struct {
	txManager *database.TxManager
	audit     audit.TxRecorder
	outbox    WebhookTxDispatcher
}

var _ filesvc.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators. outbox may be nil;
// TxRepos.Outbox is then nil and services dispatch webhooks after commit.
func NewTransactor(txManager *database.TxManager, recorder audit.TxRecorder, outbox WebhookTxDispatcher) *Transactor {
	return &Transactor{txManager: txManager, audit: recorder, outbox: outbox}
}

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx filesvc.TxRepos) error) error {
	return t.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		repos := filesvc.TxRepos{
			Files: NewFileRepository(tx),
			Audit: txAudit{recorder: t.audit, tx: tx},
		}
		if t.outbox != nil {
			repos.Outbox = txOutbox{dispatcher: t.outbox, tx: tx}
		}
		return fn(ctx, repos)
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

// txOutbox enqueues webhook events through the open transaction.
type txOutbox struct {
	dispatcher WebhookTxDispatcher
	tx         pgx.Tx
}

func (o txOutbox) Enqueue(ctx context.Context, events ...webhookcontracts.Event) error {
	return o.dispatcher.DispatchTx(ctx, o.tx, events...)
}
