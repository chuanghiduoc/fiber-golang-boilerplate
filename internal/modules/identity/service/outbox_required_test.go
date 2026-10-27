package service

import (
	"context"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TestSendVerificationRequiresTransactionalOutbox proves the verification
// service fails closed instead of falling back to an untracked goroutine when
// the transaction has no email outbox.
func TestSendVerificationRequiresTransactionalOutbox(t *testing.T) {
	db := newMemDB()
	svc := NewEmailVerificationService(VerificationDeps{Stores: db.stores(), Tx: memTx{db}})
	ctx := tenant.WithIdentity(context.Background(), tenant.DefaultID, "default")

	if err := svc.SendVerification(ctx, testID(9), "outbox@example.com"); err == nil {
		t.Fatal("SendVerification with no transactional outbox must fail")
	}
}
