package apikey

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// A ban stamps CredentialsRevokedAt and unban keeps it, so a key issued at or
// before the ban must stay dead after the owner is restored, while a key
// issued after the restore works.
func TestVerifier_RejectsKeysIssuedBeforeCredentialRevocation(t *testing.T) {
	credentials, key := newTestKey(t)
	cases := map[string]struct {
		revokedAt time.Time
		wantOK    bool
	}{
		"issued before the ban": {revokedAt: key.CreatedAt.Add(time.Hour), wantOK: false},
		"issued at the ban":     {revokedAt: key.CreatedAt, wantOK: false},
		"issued after unban":    {revokedAt: key.CreatedAt.Add(-time.Hour), wantOK: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{key: key}
			revokedAt := tc.revokedAt
			verifier := NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
				ID: testID(42), TenantID: testID(1), Role: authz.RoleMember,
				TenantStatus:         identitycontracts.TenantStatusActive,
				CredentialsRevokedAt: &revokedAt,
			}})

			_, err := verifier.Verify(context.Background(), credentials.Key, uuid.Nil)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("err = %v, want success for a key issued after the revocation", err)
				}
				return
			}
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || resolvedStatus(appErr) != fiber.StatusUnauthorized {
				t.Fatalf("err = %v, want 401 for a key issued before the revocation", err)
			}
			if len(store.touched) != 0 {
				t.Fatal("a rejected key must not update last_used_at")
			}
		})
	}
}
