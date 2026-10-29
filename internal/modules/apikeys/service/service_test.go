package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

var (
	testTenantA = testID(1)
	testTenantB = testID(2)
	testUser    = testID(10)
	testOther   = testID(20)
)

// testID returns a deterministic UUIDv7-shaped fixture id.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

type fakeRepo struct {
	keys   map[uuid.UUID]*APIKey
	nextID int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{keys: make(map[uuid.UUID]*APIKey), nextID: 1}
}

func (r *fakeRepo) Create(ctx context.Context, params NewKey) (*APIKey, error) {
	now := time.Now().UTC()
	key := &APIKey{
		ID:        testID(r.nextID),
		TenantID:  tenant.IDFromContext(ctx),
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

func (r *fakeRepo) GetByID(ctx context.Context, id uuid.UUID) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.IDFromContext(ctx) {
		return nil, apperror.ErrNotFound
	}
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) GetByHash(_ context.Context, hash []byte) (*APIKey, error) {
	for _, key := range r.keys {
		if bytes.Equal(key.Hash, hash) {
			clone := *key
			return &clone, nil
		}
	}
	return nil, apperror.ErrNotFound
}

func (r *fakeRepo) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]APIKey, error) {
	return r.list(ctx, nil, cur), nil
}

func (r *fakeRepo) ListByUserCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]APIKey, error) {
	return r.list(ctx, &userID, cur), nil
}

func (r *fakeRepo) list(ctx context.Context, userID *uuid.UUID, cur pagination.DBCursor) []APIKey {
	keys := make([]APIKey, 0)
	for _, key := range r.keys {
		if key.TenantID != tenant.IDFromContext(ctx) {
			continue
		}
		if userID != nil && key.UserID != *userID {
			continue
		}
		keys = append(keys, *key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
			return keys[i].ID.String() > keys[j].ID.String()
		}
		return keys[i].CreatedAt.After(keys[j].CreatedAt)
	})
	limit := int(cur.Limit)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys
}

func (r *fakeRepo) Update(ctx context.Context, id uuid.UUID, name string, scopes []authz.Permission) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.IDFromContext(ctx) || key.RevokedAt != nil {
		return nil, apperror.ErrNotFound
	}
	key.Name = name
	key.Scopes = append([]authz.Permission(nil), scopes...)
	key.UpdatedAt = time.Now().UTC()
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) Revoke(ctx context.Context, id uuid.UUID) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.IDFromContext(ctx) || key.RevokedAt != nil {
		return nil, apperror.ErrNotFound
	}
	now := time.Now().UTC()
	key.RevokedAt = &now
	key.UpdatedAt = now
	clone := *key
	return &clone, nil
}

func (r *fakeRepo) MarkRotated(ctx context.Context, id uuid.UUID, graceSeconds int32) (*APIKey, error) {
	key, ok := r.keys[id]
	if !ok || key.TenantID != tenant.IDFromContext(ctx) || key.RevokedAt != nil || key.RotatedAt != nil {
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

func (r *fakeRepo) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	if key, ok := r.keys[id]; ok {
		key.LastUsedAt = &at
	}
	return nil
}

type fakePrincipals struct {
	users map[uuid.UUID]identitycontracts.Principal
}

func (p fakePrincipals) GetPrincipal(_ context.Context, tenantID, userID uuid.UUID) (identitycontracts.Principal, error) {
	user, ok := p.users[userID]
	if !ok || user.TenantID != tenantID {
		return identitycontracts.Principal{}, apperror.ErrNotFound
	}
	return user, nil
}

func (p fakePrincipals) LockPrincipal(ctx context.Context, tenantID, userID uuid.UUID) (identitycontracts.Principal, error) {
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
func testContext(tenantID uuid.UUID) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), tenantID))
}

func TestService_NoSubjectNoSystemMarkerIsDenied(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	_, err := svc.Create(tenant.WithID(context.Background(), testTenantA), testUser, CreateInput{
		Name: "x", Scopes: []string{"files:read"},
	})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 403 || appErr.Code != string(apperror.CodePermissionDenied) {
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
	return NewService(repo, principals, nil, fakeTx{keys: repo, audit: recorder, principals: principals}, testAuthorizer, 8760*time.Hour)
}

// fakeRanks records the rank checks the service asks identity for. err is
// returned for every target, modelling an admin/owner that does not outrank
// the key's owner.
type fakeRanks struct {
	err   error
	calls []uuid.UUID
}

func (r *fakeRanks) CanManageMember(_ context.Context, targetUserID uuid.UUID) error {
	r.calls = append(r.calls, targetUserID)
	return r.err
}

func TestService_CreateAppliesMaxLifetime(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	const maxLifetime = 2 * time.Hour
	svc := NewService(repo, principals, nil, fakeTx{keys: repo, audit: auditWriterFunc(func(context.Context, audit.Event) error { return nil }), principals: principals}, testAuthorizer, maxLifetime)
	svc.(*service).now = func() time.Time { return now }
	ctx := testContext(testTenantA)

	created, err := svc.Create(ctx, testUser, CreateInput{Name: "defaulted", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created.ExpiresAt.Equal(now.Add(maxLifetime)) {
		t.Fatalf("expiresAt = %v, want now + max (%v)", created.ExpiresAt, now.Add(maxLifetime))
	}

	explicit := now.Add(time.Hour)
	created, err = svc.Create(ctx, testUser, CreateInput{Name: "explicit", Scopes: []string{"files:read"}, ExpiresAt: &explicit})
	if err != nil {
		t.Fatalf("create explicit: %v", err)
	}
	if !created.ExpiresAt.Equal(explicit) {
		t.Fatalf("expiresAt = %v, want %v", created.ExpiresAt, explicit)
	}

	tooLate := now.Add(maxLifetime + time.Second)
	_, err = svc.Create(ctx, testUser, CreateInput{Name: "too-late", Scopes: []string{"files:read"}, ExpiresAt: &tooLate})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
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
		original time.Time
		want     time.Time
	}{
		{name: "earlier expiry is preserved", original: now.Add(6 * time.Hour), want: now.Add(6 * time.Hour)},
		{name: "later expiry is capped", original: now.Add(240 * time.Hour), want: now.Add(maxLifetime)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
				testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
			}}
			svc := NewService(repo, principals, nil, fakeTx{keys: repo, audit: auditWriterFunc(func(context.Context, audit.Event) error { return nil }), principals: principals}, testAuthorizer, maxLifetime)
			svc.(*service).now = func() time.Time { return now }
			ctx := testContext(testTenantA)

			created, err := svc.Create(ctx, testUser, CreateInput{Name: "legacy", Scopes: []string{"files:read"}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			repo.keys[created.ID].ExpiresAt = tc.original

			rotated, err := svc.Rotate(ctx, testUser, created.ID)
			if err != nil {
				t.Fatalf("rotate: %v", err)
			}
			if !rotated.ExpiresAt.Equal(tc.want) {
				t.Fatalf("successor expiresAt = %v, want %v", rotated.ExpiresAt, tc.want)
			}
		})
	}
}

func TestService_CreateReturnsSecretOnceAndStoresOnlyHash(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	created, err := svc.Create(testContext(testTenantA), testUser, CreateInput{
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
	if !bytes.Equal(stored.Hash, apikey.Hash(created.Secret)) {
		t.Fatal("stored hash does not match the returned key")
	}
	if strings.Contains(string(stored.Hash), created.Secret) {
		t.Fatal("plaintext key must never be stored")
	}
	if stored.TenantID != testTenantA || stored.UserID != testUser {
		t.Fatalf("stored binding = tenant %s user %s, want %s/%s", stored.TenantID, stored.UserID, testTenantA, testUser)
	}
	if len(stored.Hash) != apikey.KeyHashBytes {
		t.Fatalf("stored hash length = %d, want %d bytes", len(stored.Hash), apikey.KeyHashBytes)
	}

	list, _, err := svc.List(testContext(testTenantA), 10, "")
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
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(testTenantA)

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
			_, err := svc.Create(ctx, testUser, tt.req)
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
				t.Fatalf("error = %v, want 400", err)
			}
		})
	}

	_, err := svc.Create(ctx, testID(99), CreateInput{Name: "x", Scopes: []string{"files:read"}})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 404 {
		t.Fatalf("missing owner error = %v, want 404", err)
	}

	// An API-key caller cannot grant a scope that its own key does not hold.
	scopedCaller := authz.WithSubject(testContext(testTenantA), authz.NewSubjectWithPermissions(
		testUser, testTenantA, authz.RoleAdmin, testScopeFilesRead,
	))
	_, err = svc.Create(scopedCaller, testUser, CreateInput{Name: "x", Scopes: []string{"files:write"}})
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("caller escalation error = %v, want 400", err)
	}

	// Rotation mints a secret with the existing scopes, so a narrower caller
	// cannot rotate a broader key either.
	broad, err := svc.Create(testContext(testTenantA), testUser, CreateInput{Name: "broad", Scopes: []string{"files:write"}})
	if err != nil {
		t.Fatalf("create broad key: %v", err)
	}
	_, err = svc.Rotate(scopedCaller, testUser, broad.ID)
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("rotate escalation error = %v, want 400", err)
	}
}

func TestService_OwnerHasPermissionFailsClosedWithoutRole(t *testing.T) {
	svc := &service{authorizer: testAuthorizer}
	tests := []struct {
		name      string
		principal identitycontracts.Principal
		want      bool
	}{
		{
			name:      "empty principal",
			principal: identitycontracts.Principal{},
			want:      false,
		},
		{
			name:      "known role without explicit permissions",
			principal: identitycontracts.Principal{Role: authz.RoleMember},
			want:      true,
		},
		{
			name:      "unknown role without explicit permissions",
			principal: identitycontracts.Principal{Role: "unknown"},
			want:      false,
		},
		{
			name:      "explicit permission without role",
			principal: identitycontracts.Principal{Permissions: []string{string(testScopeFilesRead)}},
			want:      true,
		},
		{
			name:      "explicit empty permission set overrides role",
			principal: identitycontracts.Principal{Role: authz.RoleMember, Permissions: []string{}},
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := svc.ownerHasPermission(tt.principal, testScopeFilesRead); got != tt.want {
				t.Fatalf("ownerHasPermission(%+v) = %v, want %v", tt.principal, got, tt.want)
			}
		})
	}
}

func TestService_CreateRejectsScopesWhenOwnerPrincipalHasNoRole(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	_, err := svc.Create(testContext(testTenantA), testUser, CreateInput{
		Name: "no-role", Scopes: []string{"files:read"},
	})
	if !isBadRequest(err) {
		t.Fatalf("create error = %v, want 400", err)
	}
	if len(repo.keys) != 0 {
		t.Fatal("a principal without a role must not mint a key")
	}
}

func TestService_RotateOnlyIssuesSecretsToTheOwner(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser:  {ID: testUser, TenantID: testTenantA, Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive},
		testOther: {ID: testOther, TenantID: testTenantA, Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(testTenantA)

	created, err := svc.Create(ctx, testUser, CreateInput{Name: "owner-key", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Another admin in the same tenant cannot obtain a secret for this key.
	if _, err := svc.Rotate(ctx, testOther, created.ID); !isNotFound(err) {
		t.Fatalf("cross-owner rotate error = %v, want 404", err)
	}
	if repo.keys[created.ID].RotatedAt != nil {
		t.Fatal("a refused cross-owner rotation must not mark the key rotated")
	}
	if len(repo.keys) != 1 {
		t.Fatalf("keys = %d, want no successor key", len(repo.keys))
	}

	rotated, err := svc.Rotate(ctx, testUser, created.ID)
	if err != nil {
		t.Fatalf("owner rotate: %v", err)
	}
	if rotated.UserID != testUser {
		t.Fatalf("successor owner = %s, want the caller %s", rotated.UserID, testUser)
	}
}

func TestService_TenantIsolation(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser:  {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
		testOther: {ID: testOther, TenantID: testTenantB, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)

	keyA, err := svc.Create(testContext(testTenantA), testUser, CreateInput{Name: "a", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	keyB, err := svc.Create(testContext(testTenantB), testOther, CreateInput{Name: "b", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}

	if _, err := svc.Get(testContext(testTenantA), keyB.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant get error = %v, want 404", err)
	}
	if _, err := svc.Update(testContext(testTenantA), keyB.ID, UpdateInput{Name: ptrString("hijack")}); !isNotFound(err) {
		t.Fatalf("cross-tenant update error = %v, want 404", err)
	}
	if err := svc.Revoke(testContext(testTenantA), keyB.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant revoke error = %v, want 404", err)
	}
	if _, err := svc.Rotate(testContext(testTenantA), testUser, keyB.ID); !isNotFound(err) {
		t.Fatalf("cross-tenant rotate error = %v, want 404", err)
	}

	listA, _, err := svc.List(testContext(testTenantA), 10, "")
	if err != nil {
		t.Fatalf("list tenant A: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != keyA.ID {
		t.Fatalf("tenant A list = %+v, want only key %s", listA, keyA.ID)
	}
	listB, _, err := svc.List(testContext(testTenantB), 10, "")
	if err != nil {
		t.Fatalf("list tenant B: %v", err)
	}
	if len(listB) != 1 || listB[0].ID != keyB.ID {
		t.Fatalf("tenant B list = %+v, want only key %s", listB, keyB.ID)
	}
}

func TestService_UpdateRevokeAndRotation(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(testTenantA)

	created, err := svc.Create(ctx, testUser, CreateInput{
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

	rotated, err := svc.Rotate(ctx, testUser, created.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.PreviousKeyID != created.ID {
		t.Fatalf("previous key id = %s, want %s", rotated.PreviousKeyID, created.ID)
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

	if _, err := svc.Rotate(ctx, testUser, created.ID); !isConflict(err) {
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
	if _, err := svc.Rotate(ctx, testUser, rotated.ID); !isConflict(err) {
		t.Fatalf("rotate revoked error = %v, want 409", err)
	}
}

func TestService_RotationGraceIsAcceptedByVerifier(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	ctx := testContext(testTenantA)

	created, err := svc.Create(ctx, testUser, CreateInput{Name: "deploy", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rotated, err := svc.Rotate(ctx, testUser, created.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	verifier := apikey.NewVerifier(verifierStore{repo: repo}, principals)
	if _, err := verifier.Verify(ctx, created.Secret, testTenantA); err != nil {
		t.Fatalf("old key must remain valid during grace: %v", err)
	}
	if _, err := verifier.Verify(ctx, rotated.Secret, testTenantA); err != nil {
		t.Fatalf("new key must verify: %v", err)
	}
	if _, err := verifier.Verify(ctx, created.Secret, testTenantB); !isForbidden(err) {
		t.Fatalf("tenant mismatch error = %v, want 403", err)
	}
}

func TestService_AnotherMembersKeyRequiresHigherRank(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, Permissions: []string{"files:read", "files:write"}, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	ranks := &fakeRanks{}
	svc := NewService(repo, principals, ranks, fakeTx{keys: repo, audit: auditWriterFunc(func(context.Context, audit.Event) error { return nil }), principals: principals}, testAuthorizer, 8760*time.Hour)

	created, err := svc.Create(testContext(testTenantA), testUser, CreateInput{Name: "owner-key", Scopes: []string{"files:read", "files:write"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	adminCtx := authz.WithSubject(testContext(testTenantA), authz.NewSubjectWithPermissions(
		testOther, testTenantA, authz.RoleAdmin, authz.Permission("files:read"), authz.Permission("files:write"),
	))
	ranks.err = apperror.NewCode(apperror.CodePermissionDenied, "cannot manage a member with an equal or higher role")
	if _, err := svc.Update(adminCtx, created.ID, UpdateInput{Name: ptrString("hijack")}); !hasStatus(err, 403) {
		t.Fatalf("equal-rank update error = %v, want 403", err)
	}
	if err := svc.Revoke(adminCtx, created.ID); !hasStatus(err, 403) {
		t.Fatalf("equal-rank revoke error = %v, want 403", err)
	}
	if repo.keys[created.ID].RevokedAt != nil || repo.keys[created.ID].Name != "owner-key" {
		t.Fatal("a refused manager changed the key")
	}

	// Outranking the owner allows managing the key, but only narrowing scopes.
	ranks.err = nil
	if _, err := svc.Update(adminCtx, created.ID, UpdateInput{Name: ptrString("managed")}); err != nil {
		t.Fatalf("outranking update: %v", err)
	}
	if _, err := svc.Update(adminCtx, created.ID, UpdateInput{Scopes: []string{"files:read", "files:write"}}); err != nil {
		t.Fatalf("same scopes are a narrowing no-op: %v", err)
	}
	if _, err := svc.Update(adminCtx, created.ID, UpdateInput{Scopes: []string{"files:write"}}); err != nil {
		t.Fatalf("narrowing update: %v", err)
	}
	if _, err := svc.Update(adminCtx, created.ID, UpdateInput{Scopes: []string{"files:read", "files:write"}}); !hasStatus(err, 403) {
		t.Fatalf("widening by another member error = %v, want 403", err)
	}
	if got := repo.keys[created.ID].Scopes; len(got) != 1 || got[0] != "files:write" {
		t.Fatalf("scopes = %v, want the narrowed files:write", got)
	}
	if err := svc.Revoke(adminCtx, created.ID); err != nil {
		t.Fatalf("outranking revoke: %v", err)
	}
}

func TestService_OwnerMayWidenOwnKeyScopes(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, Permissions: []string{"files:read", "files:write"}, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	ranks := &fakeRanks{}
	svc := NewService(repo, principals, ranks, fakeTx{keys: repo, audit: auditWriterFunc(func(context.Context, audit.Event) error { return nil }), principals: principals}, testAuthorizer, 8760*time.Hour)
	created, err := svc.Create(testContext(testTenantA), testUser, CreateInput{Name: "own-key", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ownerCtx := authz.WithSubject(testContext(testTenantA), authz.NewSubjectWithPermissions(
		testUser, testTenantA, authz.RoleMember, authz.Permission("files:read"), authz.Permission("files:write"),
	))
	updated, err := svc.Update(ownerCtx, created.ID, UpdateInput{Scopes: []string{"files:read", "files:write"}})
	if err != nil {
		t.Fatalf("owner widening: %v", err)
	}
	if len(updated.Scopes) != 2 {
		t.Fatalf("scopes = %v, want both", updated.Scopes)
	}
	if len(ranks.calls) != 0 {
		t.Fatalf("the owner's own key must not need a rank check: %v", ranks.calls)
	}
}

func TestService_RotateExpiredKeyIsConflict(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser: {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	svc := newTestService(repo, principals)
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	svc.(*service).now = func() time.Time { return now }
	ctx := testContext(testTenantA)

	created, err := svc.Create(ctx, testUser, CreateInput{Name: "short-lived", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svc.(*service).now = func() time.Time { return now.Add(25 * time.Hour) }
	repo.keys[created.ID].ExpiresAt = now.Add(time.Hour)
	if _, err := svc.Rotate(ctx, testUser, created.ID); !isConflict(err) {
		t.Fatalf("expired rotate error = %v, want 409", err)
	}
	if len(repo.keys) != 1 {
		t.Fatalf("keys = %d, want no successor for an expired key", len(repo.keys))
	}
	if repo.keys[created.ID].RotatedAt != nil {
		t.Fatal("a refused rotation must not mark the key rotated")
	}
}

type verifierStore struct {
	repo *fakeRepo
}

func (s verifierStore) FindByHash(ctx context.Context, hash []byte) (apikey.Key, error) {
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
		ExpiresAt:          &key.ExpiresAt,
		RevokedAt:          key.RevokedAt,
		RotatedAt:          key.RotatedAt,
		RotationGraceUntil: key.RotationGraceUntil,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
	}, nil
}

func (s verifierStore) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return s.repo.TouchLastUsed(ctx, id, at)
}

func isNotFound(err error) bool   { return hasStatus(err, 404) }
func isConflict(err error) bool   { return hasStatus(err, 409) }
func isBadRequest(err error) bool { return hasStatus(err, 400) }
func isForbidden(err error) bool  { return hasStatus(err, 403) }

func hasStatus(err error, status int) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && resolvedStatus(appErr) == status
}

func ptrString(value string) *string     { return &value }
func ptrTime(value time.Time) *time.Time { return &value }

// newMemberScopedService wires a rank checker so cross-member paths can be
// exercised without a database.
func newMemberScopedService(repo *fakeRepo, principals fakePrincipals, ranks *fakeRanks) Service {
	recorder := auditWriterFunc(func(context.Context, audit.Event) error { return nil })
	return NewService(repo, principals, ranks, fakeTx{keys: repo, audit: recorder, principals: principals}, testAuthorizer, 8760*time.Hour)
}

func TestService_OwnedViewsConcealAnotherMembersKey(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser:  {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
		testOther: {ID: testOther, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	ranks := &fakeRanks{}
	svc := newMemberScopedService(repo, principals, ranks)
	ctx := testContext(testTenantA)

	created, err := svc.Create(ctx, testOther, CreateInput{Name: "other", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The owner's page never contains another member's key.
	keys, _, err := svc.ListOwnedBy(ctx, testUser, 10, "")
	if err != nil {
		t.Fatalf("list owned: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("owned list = %+v, want only the caller's keys", keys)
	}
	keys, _, err = svc.ListOwnedBy(ctx, testOther, 10, "")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != created.ID {
		t.Fatalf("other list = %+v, want the owner's key", keys)
	}
	if _, _, err := svc.ListForMember(ctx, testID(999), 10, ""); !isNotFound(err) {
		t.Fatalf("unknown member list = %v, want 404", err)
	}

	if _, err := svc.GetOwnedBy(ctx, testUser, created.ID); !isNotFound(err) {
		t.Fatalf("get another member's key = %v, want 404", err)
	}
	name := "hijack"
	if _, err := svc.UpdateOwnedBy(ctx, testUser, created.ID, UpdateInput{Name: &name}); !isNotFound(err) {
		t.Fatalf("update another member's key = %v, want 404", err)
	}
	if err := svc.RevokeOwnedBy(ctx, testUser, created.ID); !isNotFound(err) {
		t.Fatalf("revoke another member's key = %v, want 404", err)
	}
	if repo.keys[created.ID].Name != "other" {
		t.Fatalf("another member's key was modified: %+v", repo.keys[created.ID])
	}
	if repo.keys[created.ID].RevokedAt != nil {
		t.Fatal("another member's key was revoked through the self-service path")
	}
}

func TestService_MemberScopedMutationsCheckPathOwnerAndRank(t *testing.T) {
	repo := newFakeRepo()
	principals := fakePrincipals{users: map[uuid.UUID]identitycontracts.Principal{
		testUser:  {ID: testUser, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
		testOther: {ID: testOther, TenantID: testTenantA, Role: authz.RoleMember, TenantStatus: identitycontracts.TenantStatusActive},
	}}
	ranks := &fakeRanks{}
	svc := newMemberScopedService(repo, principals, ranks)
	ctx := authz.WithSubject(testContext(testTenantA), authz.NewSubject(testUser, testTenantA, authz.RoleAdmin))

	created, err := svc.Create(testContext(testTenantA), testOther, CreateInput{Name: "other", Scopes: []string{"files:read"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The member id on the path must match the key's owner.
	name := "renamed"
	if _, err := svc.UpdateForMember(ctx, testUser, created.ID, UpdateInput{Name: &name}); !isNotFound(err) {
		t.Fatalf("wrong member id = %v, want 404", err)
	}
	if len(ranks.calls) != 0 {
		t.Fatal("a mismatched member id must not reach the rank checker")
	}

	updated, err := svc.UpdateForMember(ctx, testOther, created.ID, UpdateInput{Name: &name})
	if err != nil {
		t.Fatalf("member update: %v", err)
	}
	if updated.Name != name || len(ranks.calls) != 1 || ranks.calls[0] != testOther {
		t.Fatalf("updated = %+v, rank calls = %v", updated, ranks.calls)
	}

	if err := svc.RevokeForMember(ctx, testOther, created.ID); err != nil {
		t.Fatalf("member revoke: %v", err)
	}
	if repo.keys[created.ID].RevokedAt == nil {
		t.Fatal("member revoke did not persist")
	}
}
