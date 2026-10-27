package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

// Transactor implements service.Transactor: one pgx transaction binds the
// feature-flag repository and the Tier 1 audit row, so a flag mutation and its
// audit event commit or roll back together.
type Transactor struct {
	tx    *database.TxManager
	audit audit.TxRecorder
}

var _ featuressvc.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators.
func NewTransactor(tx *database.TxManager, recorder audit.TxRecorder) *Transactor {
	return &Transactor{tx: tx, audit: recorder}
}

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx featuressvc.TxRepos) error) error {
	return t.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return fn(ctx, featuressvc.TxRepos{
			Flags: New(tx),
			Audit: txAudit{recorder: t.audit, tx: tx},
		})
	})
}

// txAudit records a Tier 1 event through the caller's transaction.
type txAudit struct {
	recorder audit.TxRecorder
	tx       pgx.Tx
}

func (a txAudit) RecordTx(ctx context.Context, event audit.Event) error {
	return audit.RecordInTx(ctx, a.recorder, a.tx, event)
}
