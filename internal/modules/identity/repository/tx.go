package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/google/uuid"
)

// NewStores binds every identity repository to db (the pool, or a pgx.Tx). A
// ban hasher may be supplied; the ban paths fail loudly when one is missing.
func NewStores(db identitysqlc.DBTX, bans ...identityservice.BanHasher) identityservice.Stores {
	stores := identityservice.Stores{
		Users:         NewUserRepository(db),
		Memberships:   NewMembershipRepository(db),
		Sessions:      NewSessionRepository(db),
		RefreshTokens: NewRefreshTokenRepository(db),
		Resets:        NewPasswordResetRepository(db),
		Verifications: NewVerificationRepository(db),
		Invitations:   NewInvitationRepository(db),
		Tenants:       NewTenantRepository(db),
		MFA:           NewMFARepository(db),
		Passkeys:      NewPasskeyRepository(db),
		Privacy:       NewDataExportRepository(db),
		Roles:         NewRoleRepository(db),
	}
	if len(bans) > 0 {
		stores.Bans = bans[0]
	}
	return stores
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
	queue     *jobs.Queue
	bans      identityservice.BanHasher
}

var _ identityservice.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators without a job queue.
func NewTransactor(txManager *database.TxManager, recorder audit.TxRecorder, mail TxEmailSender, bus *events.Bus, bans ...identityservice.BanHasher) *Transactor {
	return NewTransactorWithQueue(txManager, recorder, mail, bus, nil, bans...)
}

// NewTransactorWithQueue also binds the job outbox, so privacy requests enqueue
// their durable work in the same transaction. Production wiring always passes
// the outbox sender; a nil mail is only a minimal test fixture. A nil bus
// publishes nothing. The
// ban hasher is propagated to every transaction's stores so a ban mutation can
// compute its keyed marker on the same connection.
func NewTransactorWithQueue(txManager *database.TxManager, recorder audit.TxRecorder, mail TxEmailSender, bus *events.Bus, queue *jobs.Queue, bans ...identityservice.BanHasher) *Transactor {
	t := &Transactor{txManager: txManager, audit: recorder, mail: mail, bus: bus, queue: queue}
	if len(bans) > 0 {
		t.bans = bans[0]
	}
	return t
}

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx identityservice.TxRepos) error) error {
	var afterCommit []func()
	err := t.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		repos := identityservice.TxRepos{
			Stores: NewStores(tx, t.bans),
			Audit:  txAudit{recorder: t.audit, tx: tx},
			Events: txEvents{bus: t.bus, tx: tx},
			// The callback list only runs after WithTx commits, so a
			// post-commit fallback (for example an email sent outside the
			// outbox) can never fire on a rollback.
			AfterCommit: func(f func()) {
				if f != nil {
					afterCommit = append(afterCommit, f)
				}
			},
		}
		if t.mail != nil {
			repos.Mail = txMail{sender: t.mail, tx: tx}
		}
		if t.queue != nil {
			repos.Jobs = txJobs{queue: t.queue, tx: tx}
		}
		return fn(ctx, repos)
	})
	if err != nil {
		return err
	}
	for _, f := range afterCommit {
		f()
	}
	return nil
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

// txMail enqueues the email.send.encrypted job through the open transaction.
type txMail struct {
	sender TxEmailSender
	tx     pgx.Tx
}

func (m txMail) Send(ctx context.Context, msg email.Message) error {
	return m.sender.SendTx(ctx, m.tx, msg)
}

// txJobs enqueues privacy jobs through the open transaction.
type txJobs struct {
	queue *jobs.Queue
	tx    pgx.Tx
}

func (j txJobs) Enqueue(ctx context.Context, job jobs.Job) (uuid.UUID, error) {
	return j.queue.EnqueueTx(ctx, j.tx, job)
}
