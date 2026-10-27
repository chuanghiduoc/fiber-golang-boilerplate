package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

// Transactor implements service.Transactor: InTx binds the key repository, the
// Tier 1 audit recorder, and the owner locker to one pgx transaction, so the
// service never sees pgx.Tx.
type Transactor struct {
	txManager  *database.TxManager
	audit      audit.TxRecorder
	principals identitycontracts.PrincipalReader
}

var _ apikeysvc.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators. principals must
// implement LockPrincipalTx to keep the owner lock; otherwise the service
// refuses the key write (fail closed).
func NewTransactor(txManager *database.TxManager, recorder audit.TxRecorder, principals identitycontracts.PrincipalReader) *Transactor {
	return &Transactor{txManager: txManager, audit: recorder, principals: principals}
}

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx apikeysvc.TxRepos) error) error {
	return t.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		repos := apikeysvc.TxRepos{
			Keys:  NewAPIKeyRepository(tx),
			Audit: txAudit{recorder: t.audit, tx: tx},
		}
		if locker, ok := t.principals.(principalLocker); ok {
			repos.Principals = txPrincipals{locker: locker, tx: tx}
		}
		return fn(ctx, repos)
	})
}

// principalLocker is the transactional side of the identity principal reader
// (identity's adapter implements it).
type principalLocker interface {
	LockPrincipalTx(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (identitycontracts.Principal, error)
}

// txPrincipals exposes the owner lock without leaking pgx.Tx to the service.
type txPrincipals struct {
	locker principalLocker
	tx     pgx.Tx
}

func (p txPrincipals) LockPrincipal(ctx context.Context, tenantID, userID uuid.UUID) (identitycontracts.Principal, error) {
	return p.locker.LockPrincipalTx(ctx, p.tx, tenantID, userID)
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
