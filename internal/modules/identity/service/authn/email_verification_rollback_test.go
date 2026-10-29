package authn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

type verificationTokenStore struct {
	tokens map[string]*core.VerificationToken
}

func (s *verificationTokenStore) Create(context.Context, uuid.UUID, string, []byte, time.Time) error {
	return errors.New("not used")
}

func (s *verificationTokenStore) Consume(_ context.Context, tokenHash []byte) (*core.VerificationToken, error) {
	token, ok := s.tokens[string(tokenHash)]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	delete(s.tokens, string(tokenHash))
	return token, nil
}

func (s *verificationTokenStore) DeleteByUser(context.Context, uuid.UUID) error { return nil }

func (s *verificationTokenStore) snapshot() map[string]*core.VerificationToken {
	snapshot := make(map[string]*core.VerificationToken, len(s.tokens))
	for hash, token := range s.tokens {
		cloned := *token
		snapshot[hash] = &cloned
	}
	return snapshot
}

func (s *verificationTokenStore) restore(snapshot map[string]*core.VerificationToken) {
	s.tokens = snapshot
}

type rollbackVerificationTx struct {
	stores core.Stores
	tokens *verificationTokenStore
}

func (t rollbackVerificationTx) InTx(ctx context.Context, fn func(context.Context, core.TxRepos) error) error {
	snapshot := t.tokens.snapshot()
	stores := t.stores
	stores.Verifications = t.tokens
	err := fn(ctx, core.TxRepos{Stores: stores})
	if err != nil {
		t.tokens.restore(snapshot)
	}
	return err
}

func TestVerifyEmailRollsBackConsumedTokenOnFailure(t *testing.T) {
	db := fakes.New()
	userID := fakes.TestID(88)
	db.Users[userID] = &core.User{
		ID: userID, Email: "new@example.com", Name: "Verify", Status: core.UserStatusActive,
	}
	tokenHash := hashToken("verification-token")
	tokens := &verificationTokenStore{tokens: map[string]*core.VerificationToken{
		string(tokenHash): {
			ID: fakes.TestID(99), UserID: userID, Email: "old@example.com",
			ExpiresAt: time.Now().Add(time.Hour),
		},
	}}
	stores := db.Stores()
	svc := NewEmailVerificationService(VerificationDeps{
		Stores: stores,
		Tx:     rollbackVerificationTx{stores: stores, tokens: tokens},
	})

	err := svc.Verify(context.Background(), "verification-token")
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("Verify error = %v, want 400", err)
	}
	if _, ok := tokens.tokens[string(tokenHash)]; !ok {
		t.Fatal("a failed verification must leave the token available for retry")
	}
}
