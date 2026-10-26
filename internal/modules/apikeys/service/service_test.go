package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

type fakeRepo struct {
	keys   map[int64]*APIKey
	nextID int64
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{keys: make(map[int64]*APIKey), nextID: 1}
}

func (r *fakeRepo) Create(ctx context.Context, params NewKey) (*APIKey, error) {
	now := time.Now().UTC()
	key := &APIKey{
		ID:        r.nextID,
		TenantID:  tenant.EffectiveID(ctx),
		UserID:    params.UserID,
		Name:      params.Name,
		Prefix:    params.Prefix,
		Hash:      params.Hash,
		Scopes:    append([]authz.Permission(nil), params.Scopes...),
		ExpiresAt: params.ExpiresAt,
		CreatedAt: now,
		UpdatedAt: now,
	}
	r.keys[key.ID] = key
	r.nextID++
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) GetByID(ctx context.Context, id int64) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.EffectiveID(ctx) {
		return nil, apperror.ErrNotFound
	}
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) GetByHash(_ context.Context, hash string) (*APIKey, error) {
	for _, key := range r.keys {
		if key.Hash == hash {
			clone := *key
			return &clone, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (r *fakeRepo) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]APIKey, error) {
	keys := make([]APIKey, 0)
	for _, key := range r.keys {
		if key.TenantID == tenant.EffectiveID(ctx) {
			keys = append(keys, *key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
			return keys[i].ID > keys[j].ID
		}
		return keys[i].CreatedAt.After(keys[j].CreatedAt)
	})
	limit := int(cur.Limit)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

func (r *fakeRepo) Update(ctx context.Context, id int64, name string, scopes []authz.Permission) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.EffectiveID(ctx) || key.RevokedAt != nil {
		return nil, apperror.ErrNotFound
	}
	key.Name = name
	key.Scopes = append([]authz.Permission(nil), scopes...)
	key.UpdatedAt = time.Now().UTC()
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) Revoke(ctx context.Context, id int64) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.EffectiveID(ctx) || key.RevokedAt != nil {
		return nil, apperror.ErrNotFound
	}
	now := time.Now().UTC()
	key.RevokedAt = &now
	key.UpdatedAt = now
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) MarkRotated(ctx context.Context, id int64, graceSeconds int32) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.EffectiveID(ctx) || key.RevokedAt != nil || key.RotatedAt != nil {
		return nil, apperror.ErrNotFound
	}
	now := time.Now().UTC()
	until := now.Add(time.Duration(graceSeconds) * time.Second)
	key.RotatedAt = &now
	key.RotationGraceUntil = &until
	key.UpdatedAt = now
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) TouchLastUsed(_ context.Context, id int64, at time.Time) error {
	if key, ok := r.keys[id]; ok {
		key.LastUsedAt = &at
	}
	return nil
}

type fakePrincipals struct {
	users map[int64]identitycontracts.Principal
}

func (p fakePrincipals) GetPrincipal(_ context.Context, tenantID, userID int64) (identitycontracts.Principal, error) {
	user, ok := p.users[userID]
	if !ok || user.TenantID != tenantID {
		return identitycontracts.Principal{}, apperror.ErrNotFound
	}
	return user, nil
}

func (p fakePrincipals) LockPrincipal(ctx context.Context, tenantID, userID int64) (identitycontracts.Principal, error) {
	return p.GetPrincipal(ctx, tenantID, userID)
}

type auditWriterFunc func(ctx context.Context, event audit.Event) error

func (f auditWriterFunc) RecordTx(ctx context.Context, event audit.Event) error { return f(ctx, event) }

// fakeTx runs fn directly against the fake repository. It has no rollback;
// atomicity is covered against PostgreSQL in test/integration.
type fakeTx struct {
	keys       APIKeyRepository
	audit      AuditWriter
	principals PrincipalLocker
}

var _ Transactor = fakeTx{}

func (f fakeTx) InTx(ctx context.Context, fn func(ctx context.Context, tx TxRepos) error) error {
	return fn(ctx, TxRepos{Keys: f.keys, Audit: f.audit, Principals: f.principals})
}

// testContext is a trusted system-caller context for tenantID. Tests that
// model an HTTP caller wrap it with authz.WithSubject; the subject wins.
func testContext(tenantID int64) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), tenantID))
}

func TestService_NoSubjectNoSystemMarkerIsDenied(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	_, err := svc.Create(tenant.WithID(context.Background(), 1), 10, CreateInput{
		Name: "x", Scopes: []string{"files:read"},
	})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 403 || appErr.Code != string(apperror.CodePermissionDenied) {
		t.Fatalf("error = %v, want 403 permission.denied", err)
	}
	if len(repo.keys) != 0 {
		t.Fatal("a denied create must not persist a key")
	}
}

func newTestService(repo *fakeRepo, principals fakePrincipals) Service {
	return newAuditedTestService(repo, principals, auditWriterFunc(func(context.Context, audit.Event) error { return nil }))
}

func newAuditedTestService(repo *fakeRepo, principals fakePrincipals, recorder AuditWriter) Service {
	return NewService(repo, principals, fakeTx{keys: repo, audit: recorder, principals: principals}, 8760*time.Hour)
}

func TestService_CreateAppliesMaxLifetime(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	const maxLifetime = 2 * time.Hour
	svc := NewService(repo, principals, fakeTx{keys: repo, audit: auditWriterFunc(func(context.Context, audit.Event) error { return nil }), principals: principals}, maxLifetime)
	svc.(*service).now = func() time.Time { return now }
	ctx := testContext(1)

	created, err := svc.Create(ctx, 10, CreateInput{Name: "defaulted", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(now.Add(maxLifetime)) {
		t.Fatalf("expiresAt = %v, want now + max (%v)", created.ExpiresAt, now.Add(maxLifetime))
	}

	explicit := now.Add(time.Hour)
	created, err = svc.Create(ctx, 10, CreateInput{Name: "explicit", Scopes: []string{"files:read"}, ExpiresAt: &explicit})
	if err != nil {
		t.Fatalf("create explicit: %v", err)
	}
	if created.ExpiresAt == nil || !created.ExpiresAt.Equal(explicit) {
		t.Fatalf("expiresAt = %v, want %v", created.ExpiresAt, explicit)
	}

	tooLate := now.Add(maxLifetime + time.Second)
	_, err = svc.Create(ctx, 10, CreateInput{Name: "too-late", Scopes: []string{"files:read"}, ExpiresAt: &tooLate})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 400 {
		t.Fatalf("late expiry error = %v, want 400", err)
	}
	if appErr.Code != string(apikeyscatalog.CodeAPIKeyExpiryExceedsMaxLifetime) {
		t.Fatalf("late expiry code = %q, want %q", appErr.Code, apikeyscatalog.CodeAPIKeyExpiryExceedsMaxLifetime)
	}
}

func TestService_RotateInheritsBoundedExpiry(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	const maxLifetime = 24 * time.Hour

	tests := []struct {
		name     string
		original *time.Time
		want     time.Time
	}{
		{name: "legacy NULL expiry gets the maximum", original: nil, want: now.Add(maxLifetime)},
		{name: "earlier expiry is preserved", original: ptrTime(now.Add(6 * time.Hour)), want: now.Add(6 * time.Hour)},
		{name: "later legacy expiry is capped", original: ptrTime(now.Add(240 * time.Hour)), want: now.Add(maxLifetime)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
				10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
			}}
			svc := NewService(repo, principals, fakeTx{keys: repo, audit: auditWriterFunc(func(context.Context, audit.Event) error { return nil }), principals: principals}, maxLifetime)
			svc.(*service).now = func() time.Time { return now }
			ctx := testContext(1)

			created, err := svc.Create(ctx, 10, CreateInput{Name: "legacy", Scopes: []string{"files:read"}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			// Simulate a row issued before APIKEYS_MAX_LIFETIME existed.
			repo.keys[created.ID].ExpiresAt = tc.original

			rotated, err := svc.Rotate(ctx, 10, created.ID)
			if err != nil {
				t.Fatalf("rotate: %v", err)
			}
			if rotated.ExpiresAt == nil || !rotated.ExpiresAt.Equal(tc.want) {
				t.Fatalf("successor expiresAt = %v, want %v", rotated.ExpiresAt, tc.want)
			}
		})
	}
}

func TestService_CreateReturnsSecretOnceAndStoresOnlyHash(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	created, err := svc.Create(testContext(1), 10, CreateInput{
		Name:   "  deploy  ",
		Scopes: []string{"files:read", "files:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "deploy" {
		t.Fatalf("name = %q, want trimmed deploy", created.Name)
	}
	if len(created.Scopes) != 1 || created.Scopes[0] != "files:read" {
		t.Fatalf("scopes = %v, want deduplicated files:read", created.Scopes)
	}
	if !apikey.Valid(created.Secret) {
		t.Fatalf("returned key %q is malformed", created.Secret)
	}
	if !strings.HasPrefix(created.Secret, apikey.DefaultPrefix+"_") {
		t.Fatalf("returned key %q has wrong prefix", created.Secret)
	}
	stored := repo.keys[created.ID]
	if stored == nil {
		t.Fatal("key not persisted")
	}
	if stored.Hash != apikey.Hash(created.Secret) {
		t.Fatal("stored hash does not match the returned key")
	}
	if strings.Contains(stored.Hash, created.Secret) {
		t.Fatal("plaintext key must never be stored")
	}
	if stored.TenantID != 1 || stored.UserID != 10 {
		t.Fatalf("stored binding = tenant %d user %d, want 1/10", stored.TenantID, stored.UserID)
	}
	if stored.Hash == "" || len(stored.Hash) != apikey.KeyHashBytes*2 {
		t.Fatalf("stored hash = %q, want 64 hex chars", stored.Hash)
	}

	list, _, err := svc.List(testContext(1), 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	// The list DTO has no secret fields by construction; a serialized API key
	// response must not contain the plaintext.
}

func TestService_CreateRejectsInvalidRequests(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(1)

	tests := []struct {
		name string
		req  CreateInput
	}{
		{name: "blank name", req: CreateInput{Name: "  ", Scopes: []string{"files:read"}}},
		{name: "no scopes", req: CreateInput{Name: "x"}},
		{name: "unknown scope", req: CreateInput{Name: "x", Scopes: []string{"root:everything"}}},
		{name: "privilege escalation", req: CreateInput{Name: "x", Scopes: []string{"admin:users:read"}}},
		{name: "past expiry", req: CreateInput{
			Name:      "x",
			Scopes:    []string{"files:read"},
			ExpiresAt: ptrTime(time.Now().Add(-time.Minute)),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Create(ctx, 10, tt.req)
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || appErr.Status != 400 {
				t.Fatalf("error = %v, want 400", err)
			}
		})
	}

	_, err := svc.Create(ctx, 99, CreateInput{Name: "x", Scopes: []string{"files:read"}})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 404 {
		t.Fatalf("missing owner error = %v, want 404", err)
	}

	// An API-key caller cannot grant a scope that its own key does not hold.
	scopedCaller := authz.WithSubject(testContext(1), authz.NewSubjectWithPermissions(
		10, 1, authz.RoleAdmin, testScopeFilesRead,
	))
	_, err = svc.Create(scopedCaller, 10, CreateInput{Name: "x", Scopes: []string{"files:write"}})
	if !errors.As(err, &appErr) || appErr.Status != 400 {
		t.Fatalf("caller escalation error = %v, want 400", err)
	}

	// Rotation mints a secret with the existing scopes, so a narrower caller
	// cannot rotate a broader key either.
	broad, err := svc.Create(testContext(1), 10, CreateInput{Name: "broad", Scopes: []string{"files:write"}})
	if err != nil {
		t.Fatalf("create broad key: %v", err)
	}
	_, err = svc.Rotate(scopedCaller, 10, broad.ID)
	if !errors.As(err, &appErr) || appErr.Status != 400 {
		t.Fatalf("rotate escalation error = %v, want 400", err)
	}
}

func TestService_RotateOnlyIssuesSecretsToTheOwner(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive},
		20: {ID: 20, TenantID: 1, Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(1)

	created, err := svc.Create(ctx, 10, CreateInput{Name: "owner-key", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Another admin in the same tenant cannot obtain a secret for this key.
	if _, err := svc.Rotate(ctx, 20, created.ID); !isNotFound(err) {
		t.Fatalf("cross-owner rotate error = %v, want 404", err)
	}
	if repo.keys[created.ID].RotatedAt != nil {
		t.Fatal("a refused cross-owner rotation must not mark the key rotated")
	}
	if len(repo.keys) != 1 {
		t.Fatalf("keys = %d, want no successor key", len(repo.keys))
	}

	rotated, err := svc.Rotate(ctx, 10, created.ID)
	if err != nil {
		t.Fatalf("owner rotate: %v", err)
	}
	if rotated.UserID != 10 {
		t.Fatalf("successor owner = %d, want the caller 10", rotated.UserID)
	}
}

func TestService_TenantIsolation(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
		20: {ID: 20, TenantID: 2, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	keyA, err := svc.Create(testContext(1), 10, CreateInput{Name: "a", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create tenant 1: %v", err)
	}
	keyB, err := svc.Create(testContext(2), 20, CreateInput{Name: "b", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create tenant 2: %v", err)
	}

	if _, err := svc.Get(testContext(1), keyB.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant get error = %v, want 404", err)
	}
	if _, err := svc.Update(testContext(1), keyB.ID, UpdateInput{Name: ptrString("hijack")}); !isNotFound(err) {
		t.Fatalf("cross-tenant update error = %v, want 404", err)
	}
	if err := svc.Revoke(testContext(1), keyB.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant revoke error = %v, want 404", err)
	}
	if _, err := svc.Rotate(testContext(1), 10, keyB.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant rotate error = %v, want 404", err)
	}

	listA, _, err := svc.List(testContext(1), 10, "")
	if err != nil {
		t.Fatalf("list tenant 1: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != keyA.ID {
		t.Fatalf("tenant 1 list = %+v, want only key %d", listA, keyA.ID)
	}
	listB, _, err := svc.List(testContext(2), 10, "")
	if err != nil {
		t.Fatalf("list tenant 2: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != keyB.ID {
		t.Fatalf("tenant 2 list = %+v, want only key %d", listB, keyB.ID)
	}
}

func TestService_UpdateRevokeAndRotation(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(1)

	created, err := svc.Create(ctx, 10, CreateInput{
		Name:   "deploy",
		Scopes: []string{"files:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := svc.Update(ctx, created.ID, UpdateInput{
		Name:   ptrString("release"),
		Scopes: []string{"files:read", "files:write"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "release" || len(updated.Scopes) != 2 {
		t.Fatalf("updated = %+v, want release with two scopes", updated)
	}
	if _, err := svc.Update(ctx, created.ID, UpdateInput{Scopes: []string{"admin:users:read"}}); !isBadRequest(err) {
		t.Fatalf("scope escalation update error = %v, want 400", err)
	}

	rotated, err := svc.Rotate(ctx, 10, created.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.PreviousKeyID != created.ID {
		t.Fatalf("previous key id = %d, want %d", rotated.PreviousKeyID, created.ID)
	}
	if rotated.Secret == created.Secret {
		t.Fatal("rotation must return a new secret")
	}
	if rotated.ID == created.ID {
		t.Fatal("rotation must create a new key row")
	}
	if len(rotated.Scopes) != 2 || rotated.Name != "release" {
		t.Fatalf("rotated metadata = %+v, want inherited name/scopes", rotated)
	}
	old := repo.keys[created.ID]
	if old.RotatedAt == nil || old.RotationGraceUntil == nil {
		t.Fatal("old key must enter rotation grace")
	}
	if !old.RotationGraceUntil.After(time.Now()) {
		t.Fatal("rotation grace must be in the future")
	}

	if _, err := svc.Rotate(ctx, 10, created.ID); !isConflict(err) {
		t.Fatalf("second rotate error = %v, want 409", err)
	}
	if err := svc.Revoke(ctx, rotated.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := svc.Revoke(ctx, rotated.ID); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	if _, err := svc.Update(ctx, rotated.ID, UpdateInput{Name: ptrString("x")}); !isConflict(err) {
		t.Fatalf("update revoked error = %v, want 409", err)
	}
	if _, err := svc.Rotate(ctx, 10, rotated.ID); !isConflict(err) {
		t.Fatalf("rotate revoked error = %v, want 409", err)
	}
}

func TestService_RotationGraceIsAcceptedByVerifier(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[int64]identitycontracts.Principal{
		10: {ID: 10, TenantID: 1, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(1)

	created, err := svc.Create(ctx, 10, CreateInput{Name: "deploy", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rotated, err := svc.Rotate(ctx, 10, created.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	verifier := apikey.NewVerifier(verifierStore{repo: repo}, principals)
	if _, err := verifier.Verify(ctx, created.Secret, 1); err != nil {
		t.Fatalf("old key must remain valid during grace: %v", err)
	}
	if _, err := verifier.Verify(ctx, rotated.Secret, 1); err != nil {
		t.Fatalf("new key must verify: %v", err)
	}
	if _, err := verifier.Verify(ctx, created.Secret, 2); !isForbidden(err) {
		t.Fatalf("tenant mismatch error = %v, want 403", err)
	}
}

type verifierStore struct {
	repo *fakeRepo
}

func (s verifierStore) FindByHash(ctx context.Context, hash string) (apikey.Key, error) {
	key, err := s.repo.GetByHash(ctx, hash)
	if err != nil {
		return apikey.Key{}, err
	}
	return apikey.Key{
		ID:                 key.ID,
		TenantID:           key.TenantID,
		UserID:             key.UserID,
		Name:               key.Name,
		Prefix:             key.Prefix,
		Hash:               key.Hash,
		Scopes:             key.Scopes,
		ExpiresAt:          key.ExpiresAt,
		RevokedAt:          key.RevokedAt,
		RotatedAt:          key.RotatedAt,
		RotationGraceUntil: key.RotationGraceUntil,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
	}, nil
}

func (s verifierStore) TouchLastUsed(ctx context.Context, id int64, at time.Time) error {
	return s.repo.TouchLastUsed(ctx, id, at)
}

func isNotFound(err error) bool   { return hasStatus(err, 404) }
func isConflict(err error) bool   { return hasStatus(err, 409) }
func isBadRequest(err error) bool { return hasStatus(err, 400) }
func isForbidden(err error) bool  { return hasStatus(err, 403) }

func hasStatus(err error, status int) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && appErr.Status == status
}

func ptrString(value string) *string     { return &value }
func ptrTime(value time.Time) *time.Time { return &value }
