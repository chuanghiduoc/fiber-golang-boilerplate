package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	notificationservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// Transactor implements service.Transactor: InTx binds the notification
// repository and the email outbox to one pgx transaction, so the in-app row
// and its email job commit or roll back together.
type Transactor struct {
	txManager *database.TxManager
	mail      email.Sender
}

var _ notificationservice.Transactor = (*Transactor)(nil)

// NewTransactor wires the transaction-bound collaborators. A nil mail leaves
// TxRepos.Mail nil.
func NewTransactor(txManager *database.TxManager, mail email.Sender) *Transactor {
	return &Transactor{txManager: txManager, mail: mail}
}

func (t *Transactor) InTx(ctx context.Context, fn func(ctx context.Context, tx notificationservice.TxRepos) error) error {
	return t.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		repos := notificationservice.TxRepos{Notifications: NewNotificationRepository(tx)}
		if t.mail != nil {
			repos.Mail = txMail{sender: t.mail, tx: tx}
		}
		return fn(ctx, repos)
	})
}

// txEmailSender can enqueue an email on a caller-owned transaction.
// jobs.QueuedSender implements it; a plain email.Sender does not.
type txEmailSender interface {
	SendTx(ctx context.Context, tx pgx.Tx, msg email.Message) error
}

// txMail enqueues the email.send job through the open transaction when the
// sender supports it, otherwise it sends while the transaction is open.
type txMail struct {
	sender email.Sender
	tx     pgx.Tx
}

func (m txMail) Send(ctx context.Context, msg email.Message) error {
	if sender, ok := m.sender.(txEmailSender); ok {
		return sender.SendTx(ctx, m.tx, msg)
	}
	return m.sender.Send(ctx, msg)
}
