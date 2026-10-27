package authn

import (
	"context"
	"errors"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"testing"
	"time"
)

type fakeTokenPurgeRepo struct {
	backlog map[string]int64
	err     error
}

func (f *fakeTokenPurgeRepo) take(table string, batch int32) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	n := min(f.backlog[table], int64(batch))
	f.backlog[table] -= n
	return n, nil
}

func (f *fakeTokenPurgeRepo) PurgeExpiredRefreshTokens(_ context.Context, batch int32) (int64, error) {
	return f.take("refresh", batch)
}

func (f *fakeTokenPurgeRepo) PurgeExpiredPasswordResetTokens(_ context.Context, batch int32) (int64, error) {
	return f.take("reset", batch)
}

func (f *fakeTokenPurgeRepo) PurgeExpiredEmailVerificationTokens(_ context.Context, batch int32) (int64, error) {
	return f.take("verification", batch)
}

func (f *fakeTokenPurgeRepo) PurgeExpiredInvitations(_ context.Context, batch int32) (int64, error) {
	return f.take("invitations", batch)
}

func (f *fakeTokenPurgeRepo) PurgeStaleSessions(_ context.Context, revokedBefore, idleBefore time.Time, batch int32) (int64, error) {
	if !revokedBefore.Before(idleBefore) {
		return 0, errors.New("revoked sessions must be kept longer than idle ones")
	}
	return f.take("sessions", batch)
}

func TestTokenPurgerDrainsEveryTableInBatches(t *testing.T) {
	repo := &fakeTokenPurgeRepo{backlog: map[string]int64{
		"refresh":      2*tokenPurgeBatchSize + 7,
		"reset":        tokenPurgeBatchSize,
		"verification": 3,
		"invitations":  tokenPurgeBatchSize + 1,
		"sessions":     5,
	}}
	if err := NewTokenPurger(repo).HandleJob(fakes.TenantCtx(), nil); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for table, left := range repo.backlog {
		if left != 0 {
			t.Errorf("%s: %d expired rows left", table, left)
		}
	}
}

func TestTokenPurgerReturnsDatabaseErrors(t *testing.T) {
	boom := errors.New("connection reset")
	err := NewTokenPurger(&fakeTokenPurgeRepo{err: boom}).HandleJob(fakes.TenantCtx(), nil)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the database error so the job retries, got %v", err)
	}
}
