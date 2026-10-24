package apikey

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

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
				ID: 42, TenantID: 1, Role: authz.RoleUser,
				TenantStatus:         identitycontracts.TenantStatusActive,
				CredentialsRevokedAt: &revokedAt,
			}})

			_, err := verifier.Verify(context.Background(), credentials.Key, 0)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("err = %v, want success for a key issued after the revocation", err)
				}
				return
			}
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || appErr.Status != fiber.StatusUnauthorized {
				t.Fatalf("err = %v, want 401 for a key issued before the revocation", err)
			}
			if len(store.touched) != 0 {
				t.Fatal("a rejected key must not update last_used_at")
			}
		})
	}
}
