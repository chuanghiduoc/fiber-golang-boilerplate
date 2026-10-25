package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// NewStores binds every identity repository to db (the pool, or a pgx.Tx).
func NewStores(db identitysqlc.DBTX) identityservice.Stores {
	return identityservice.Stores{
		Users:         NewUserRepository(db),
		Memberships:   NewMembershipRepository(db),
		Sessions:      NewSessionRepository(db),
		RefreshTokens: NewRefreshTokenRepository(db),
		Resets:        NewPasswordResetRepository(db),
		Verifications: NewVerificationRepository(db),
		Invitations:   NewInvitationRepository(db),
		Tenants:       NewTenantRepository(db),
		Roles:         NewRoleRepository(db),
	}
}

// TxEmailSender enqueues an email on the caller's transaction.
// jobs.QueuedSender implements it.
type TxEmailSender interface {
	SendTx(ctx context.Context, tx pgx.Tx, msg email.Message) error
}

// Transactor implements service.Transactor: InTx binds the identity stores,
// the Tier 1 audit recorder, the event publisher, and the email outbox to one
// pgx transaction, so services never see pgx.Tx.
type Transactor struct {
	txManager *database.TxManager
	audit     audit.TxRecorder
	mail      TxEmailSender
	bus       *events.Bus
}

var _ identityservice.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators. mail may be nil
// (TxRepos.Mail is then nil and services send after commit). A nil bus (unit
// tests, the seeder) publishes nothing.
func NewTransactor(txManager *database.TxManager, recorder audit.TxRecorder, mail TxEmailSender, bus *events.Bus) *Transactor {
	return &Transactor{txManager: txManager, audit: recorder, mail: mail, bus: bus}
}

// MailTransactional reports whether TxRepos.Mail is set.
func (t *Transactor) MailTransactional() bool { return t.mail != nil }

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx identityservice.TxRepos) error) error {
	return t.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		repos := identityservice.TxRepos{
			Stores: NewStores(tx),
			Audit:  txAudit{recorder: t.audit, tx: tx},
			Events: txEvents{bus: t.bus, tx: tx},
		}
		if t.mail != nil {
			repos.Mail = txMail{sender: t.mail, tx: tx}
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

// txMail enqueues the email.send job through the open transaction.
type txMail struct {
	sender TxEmailSender
	tx     pgx.Tx
}

func (m txMail) Send(ctx context.Context, msg email.Message) error {
	return m.sender.SendTx(ctx, m.tx, msg)
}
