package apikey

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeStore struct {
	key            Key
	findErr        error
	findCalls      int
	touched        []time.Time
	touchedTenants []int64
}

func (s *fakeStore) FindByHash(_ context.Context, hash string) (Key, error) {
	s.findCalls++
	if s.findErr != nil {
		return Key{}, s.findErr
	}
	if hash != s.key.Hash {
		return Key{}, apperror.ErrNotFound
	}
	return s.key, nil
}

func (s *fakeStore) TouchLastUsed(ctx context.Context, id int64, at time.Time) error {
	if id != s.key.ID {
		return apperror.ErrNotFound
	}
	s.touched = append(s.touched, at)
	s.touchedTenants = append(s.touchedTenants, tenant.EffectiveID(ctx))
	lastUsed := at
	s.key.LastUsedAt = &lastUsed
	return nil
}

type fakePrincipals struct {
	principal identitycontracts.Principal
	err       error
}

func (p fakePrincipals) GetPrincipal(context.Context, int64, int64) (identitycontracts.Principal, error) {
	return p.principal, p.err
}

func newTestKey(t *testing.T) (Credentials, Key) {
	t.Helper()
	credentials, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return credentials, Key{
		ID:        7,
		TenantID:  1,
		UserID:    42,
		Name:      "ci",
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		Scopes:    []authz.Permission{testScopeRead},
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func TestGenerate_FormatAndHash(t *testing.T) {
	credentials, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(credentials.Key, DefaultPrefix+"_") {
		t.Fatalf("key = %q, want prefix %q", credentials.Key, DefaultPrefix+"_")
	}
	if prefix, err := ParsePrefix(credentials.Key); err != nil || prefix != DefaultPrefix {
		t.Fatalf("parse = %q, %v; want %q", prefix, err, DefaultPrefix)
	}
	if !Valid(credentials.Key) {
		t.Fatal("generated key must be valid")
	}
	if len(credentials.Hash) != KeyHashBytes*2 {
		t.Fatalf("hash length = %d, want %d hex characters", len(credentials.Hash), KeyHashBytes*2)
	}
	if credentials.Hash != Hash(credentials.Key) {
		t.Fatal("stored hash must be SHA-256 of the complete key")
	}
	other, err := Generate()
	if err != nil {
		t.Fatalf("generate second: %v", err)
	}
	if other.Key == credentials.Key {
		t.Fatal("two generated keys must differ")
	}
}

func TestGenerate_UsesChecksummedBase62Format(t *testing.T) {
	credentials, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	prefix, secret, ok := strings.Cut(credentials.Key, "_")
	if !ok || prefix != DefaultPrefix {
		t.Fatalf("key = %q, want prefix %q", credentials.Key, DefaultPrefix)
	}
	if len(secret) != newSecretLen {
		t.Fatalf("secret length = %d, want %d (43 base62 random + 6 checksum)", len(secret), newSecretLen)
	}
	if _, err := ParsePrefix(credentials.Key); err != nil {
		t.Fatalf("parse generated key: %v", err)
	}
}

func TestVerifier_RejectsBadChecksumBeforeLookup(t *testing.T) {
	credentials, key := newTestKey(t)
	if len(credentials.Key) < 2 {
		t.Fatal("generated key is too short")
	}
	// Flip the last checksum character to another base62 digit. The key stays
	// structurally valid but fails its CRC32 check.
	last := credentials.Key[len(credentials.Key)-1]
	replacement := byte('0')
	if last == '0' {
		replacement = '1'
	}
	corrupted := credentials.Key[:len(credentials.Key)-1] + string(replacement)
	if Valid(corrupted) {
		t.Fatalf("corrupted key %q must not be valid", corrupted)
	}

	store := &fakeStore{key: key}
	verifier := NewVerifier(store, nil)
	_, err := verifier.Verify(context.Background(), corrupted, 0)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusUnauthorized {
		t.Fatalf("error = %v, want 401", err)
	}
	if appErr.Detail != "invalid API key" {
		t.Fatalf("detail = %q, want the generic invalid API key message", appErr.Detail)
	}
	if store.findCalls != 0 {
		t.Fatalf("FindByHash calls = %d, want 0: a bad checksum must not reach the database", store.findCalls)
	}
}

// TestParsePrefix_AcceptsLegacyFormat covers keys issued before the checksum
// existed: a 32-byte base64url secret with no checksum must keep verifying.
func TestParsePrefix_AcceptsLegacyFormat(t *testing.T) {
	legacy := DefaultPrefix + "_" + base64.RawURLEncoding.EncodeToString([]byte(
		"0123456789abcdef0123456789abcdef", // exactly 32 bytes
	))
	if len(strings.SplitN(legacy, "_", 2)[1]) != legacySecretLen {
		t.Fatalf("legacy fixture length = %d", len(strings.SplitN(legacy, "_", 2)[1]))
	}
	if !Valid(legacy) {
		t.Fatal("legacy key must stay valid for verification")
	}
	prefix, err := ParsePrefix(legacy)
	if err != nil || prefix != DefaultPrefix {
		t.Fatalf("parse legacy = %q, %v", prefix, err)
	}
}

func TestGenerate_RejectsInvalidPrefix(t *testing.T) {
	for _, prefix := range []string{"", "A", "UPPER", "has space", "toolongprefixname"} {
		if _, err := GenerateWithPrefix(prefix); !errors.Is(err, ErrMalformedKey) {
			t.Errorf("GenerateWithPrefix(%q) error = %v, want ErrMalformedKey", prefix, err)
		}
	}
}

func TestParsePrefix_RejectsMalformedKeys(t *testing.T) {
	valid, _ := Generate()
	invalid := []string{
		"",
		"fgb",
		"fgb_",
		"FGB_" + strings.Repeat("a", 43),
		"fgb_short",
		"fgb_" + strings.Repeat("!", 43),
		"fgb_" + strings.Repeat("a", 42),
	}
	for _, key := range invalid {
		if Valid(key) {
			t.Errorf("key %q must be invalid", key)
		}
	}
	if !Valid(valid.Key) {
		t.Fatal("valid key rejected")
	}
}

func fixedClock(ts time.Time) Option {
	return WithClock(func() time.Time { return ts })
}

func TestVerifier_SuccessAndLastUsedTouch(t *testing.T) {
	credentials, key := newTestKey(t)
	store := &fakeStore{key: key}
	now := time.Unix(1_700_000_100, 0).UTC()
	principal := identitycontracts.Principal{ID: 42, TenantID: 1, Email: "owner@test.com", Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive}
	verifier := NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now))

	verified, err := verifier.Verify(context.Background(), credentials.Key, 1)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.Key.ID != key.ID || verified.Principal.ID != 42 {
		t.Fatalf("verified = %+v, want key 7/principal 42", verified)
	}
	if len(verified.Key.Scopes) != 1 || verified.Key.Scopes[0] != testScopeRead {
		t.Fatalf("scopes = %v, want apikeytest:read", verified.Key.Scopes)
	}
	if len(store.touched) != 1 || !store.touched[0].Equal(now) {
		t.Fatalf("touched = %v, want one write at %v", store.touched, now)
	}
	if len(store.touchedTenants) != 1 || store.touchedTenants[0] != key.TenantID {
		t.Fatalf("touch tenants = %v, want the key's tenant %d", store.touchedTenants, key.TenantID)
	}

	// No second write inside the touch interval.
	verifier.Verify(context.Background(), credentials.Key, 1)
	if len(store.touched) != 1 {
		t.Fatalf("touches = %d, want 1 within interval", len(store.touched))
	}

	// A write is attempted again once the interval has elapsed.
	verifier = NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now.Add(2*time.Minute)))
	verifier.Verify(context.Background(), credentials.Key, 1)
	if len(store.touched) != 2 {
		t.Fatalf("touches = %d, want 2 after interval", len(store.touched))
	}
}

func TestVerifier_LastUsedTouchUsesKeyTenant(t *testing.T) {
	credentials, key := newTestKey(t)
	key.TenantID = 2
	store := &fakeStore{key: key}
	now := time.Unix(1_700_000_100, 0).UTC()
	principal := identitycontracts.Principal{ID: 42, TenantID: 2, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive}
	verifier := NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now))

	// A header-less request resolves the default tenant; the write must still
	// run under the key's own tenant or RLS would update no row.
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	if _, err := verifier.Verify(ctx, credentials.Key, 0); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(store.touchedTenants) != 1 || store.touchedTenants[0] != key.TenantID {
		t.Fatalf("touch tenants = %v, want the key's tenant %d", store.touchedTenants, key.TenantID)
	}
}

func TestVerifier_RejectsInvalidStates(t *testing.T) {
	credentials, baseKey := newTestKey(t)
	now := time.Unix(1_700_000_100, 0).UTC()
	principal := identitycontracts.Principal{ID: 42, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive}

	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	pastGrace := now.Add(-time.Minute)
	futureGrace := now.Add(time.Hour)

	tests := []struct {
		name       string
		mutate     func(*Key)
		tenant     int64
		wantStatus int
	}{
		{
			name:       "expired",
			mutate:     func(k *Key) { k.ExpiresAt = &past },
			tenant:     1,
			wantStatus: 401,
		},
		{
			name:       "revoked",
			mutate:     func(k *Key) { k.RevokedAt = &past },
			tenant:     1,
			wantStatus: 401,
		},
		{
			name: "rotation grace active",
			mutate: func(k *Key) {
				k.RotatedAt = &past
				k.RotationGraceUntil = &future
			},
			tenant:     1,
			wantStatus: 200,
		},
		{
			name: "rotation grace expired",
			mutate: func(k *Key) {
				k.RotatedAt = &past
				k.RotationGraceUntil = &pastGrace
			},
			tenant:     1,
			wantStatus: 401,
		},
		{
			name: "rotated without grace",
			mutate: func(k *Key) {
				k.RotatedAt = &past
			},
			tenant:     1,
			wantStatus: 401,
		},
		{
			name:       "tenant mismatch",
			mutate:     func(*Key) {},
			tenant:     2,
			wantStatus: 403,
		},
		{
			name: "expiry extends beyond active rotation grace",
			mutate: func(k *Key) {
				k.RotatedAt = &past
				k.ExpiresAt = &future
				k.RotationGraceUntil = &futureGrace
			},
			tenant:     1,
			wantStatus: 200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := baseKey
			tt.mutate(&key)
			store := &fakeStore{key: key}
			verifier := NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now))
			_, err := verifier.Verify(context.Background(), credentials.Key, tt.tenant)
			if tt.wantStatus == 200 {
				if err != nil {
					t.Fatalf("verify: %v", err)
				}
				return
			}
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || appErr.Status != tt.wantStatus {
				t.Fatalf("error = %v, want status %d", err, tt.wantStatus)
			}
		})
	}
}

func TestVerifier_RejectsUnknownMalformedAndInactiveOwner(t *testing.T) {
	credentials, key := newTestKey(t)
	now := time.Unix(1_700_000_100, 0).UTC()

	tests := []struct {
		name       string
		raw        string
		store      *fakeStore
		principals identitycontracts.PrincipalReader
	}{
		{
			name:  "unknown hash",
			raw:   credentials.Key,
			store: &fakeStore{key: key, findErr: apperror.ErrNotFound},
		},
		{
			name:  "malformed",
			raw:   "fgb_short",
			store: &fakeStore{key: key},
		},
		{
			name:       "owner missing",
			raw:        credentials.Key,
			store:      &fakeStore{key: key},
			principals: fakePrincipals{err: apperror.ErrNotFound},
		},
		{
			name:  "owner tenant mismatch",
			raw:   credentials.Key,
			store: &fakeStore{key: key},
			principals: fakePrincipals{principal: identitycontracts.Principal{
				ID: 42, TenantID: 2, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := NewVerifier(tt.store, tt.principals, fixedClock(now))
			_, err := verifier.Verify(context.Background(), tt.raw, 0)
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || appErr.Status != 401 {
				t.Fatalf("error = %v, want 401", err)
			}
		})
	}
}

func TestVerifier_RejectsSuspendedTenant(t *testing.T) {
	credentials, key := newTestKey(t)
	store := &fakeStore{key: key}
	verifier := NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
		ID: 42, TenantID: 1, Role: authz.RoleMember, TenantStatus: "suspended",
	}})

	_, err := verifier.Verify(context.Background(), credentials.Key, 0)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401 for a suspended tenant", err)
	}
	if len(store.touched) != 0 {
		t.Fatal("a rejected key must not update last_used_at")
	}
}
