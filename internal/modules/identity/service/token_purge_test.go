package service

import (
	"context"
	"errors"
	"testing"
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

func TestTokenPurgerDrainsEveryTableInBatches(t *testing.T) {
	repo := &fakeTokenPurgeRepo{backlog: map[string]int64{
		"refresh":      2*tokenPurgeBatchSize + 7,
		"reset":        tokenPurgeBatchSize,
		"verification": 3,
	}}
	if err := NewTokenPurger(repo).HandleJob(context.Background(), nil); err != nil {
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
	err := NewTokenPurger(&fakeTokenPurgeRepo{err: boom}).HandleJob(context.Background(), nil)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the database error so the job retries, got %v", err)
	}
}
