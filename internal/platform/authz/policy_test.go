package authz

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestSubject_Permissions(t *testing.T) {
	roleSubject := NewSubject(testID(10), testID(1), RoleMember)
	if roleSubject.Has(testPermissionRead) || roleSubject.Has(PermissionAdminUsersRead) {
		t.Fatal("a bare role must not grant any permission")
	}

	scoped := NewSubjectWithPermissions(testID(10), testID(1), RoleAdmin, testPermissionRead)
	if !scoped.Has(testPermissionRead) {
		t.Fatal("explicit scope should be granted")
	}
	if scoped.Has(PermissionAdminUsersRead) {
		t.Fatal("explicit scopes must override the admin role catalog")
	}

	empty := NewSubjectWithPermissions(testID(10), testID(1), RoleAdmin)
	if empty.Permissions == nil {
		t.Fatal("explicit empty scopes must be non-nil")
	}
	if empty.Has(PermissionAdminUsersRead) || empty.Has(testPermissionRead) {
		t.Fatal("explicit empty scopes must deny everything")
	}
}

func TestSubject_AttributesAreCopied(t *testing.T) {
	base := NewSubject(testID(1), testID(2), RoleMember)
	withAttr := base.WithAttribute(AttributeAuthMethod, "api_key")

	if got := withAttr.Attribute(AttributeAuthMethod); got != "api_key" {
		t.Fatalf("attribute = %q, want api_key", got)
	}
	if base.Attributes != nil {
		t.Fatal("WithAttribute must not mutate the original subject")
	}
}

func TestSubject_ContextHelpers(t *testing.T) {
	ctx := context.Background()
	if _, ok := SubjectFromContext(ctx); ok {
		t.Fatal("empty context must not contain a subject")
	}
	subject := NewSubjectWithPermissions(testID(7), testID(3), RoleMember, testPermissionRead)
	ctx = WithSubject(ctx, subject)
	got, ok := SubjectFromContext(ctx)
	if !ok {
		t.Fatal("subject missing from context")
	}
	if got.UserID != testID(7) || got.TenantID != testID(3) || !got.Has(testPermissionRead) {
		t.Fatalf("subject = %+v, want user 7 tenant 3 files:read", got)
	}
}

func TestPolicies(t *testing.T) {
	resource := Resource{TenantID: testID(2), OwnerID: testID(9)}

	if got := (TenantPolicy{}).Evaluate(NewSubject(testID(1), testID(2), RoleMember), resource); got != Allow {
		t.Errorf("same tenant = %s, want allow", got)
	}
	if got := (TenantPolicy{}).Evaluate(NewSubject(testID(1), testID(3), RoleMember), resource); got != Conceal {
		t.Errorf("cross tenant = %s, want conceal", got)
	}
	cross := NewSubject(testID(1), testID(3), RoleAdmin)
	cross.CrossTenant = true
	if got := (TenantPolicy{}).Evaluate(cross, resource); got != Allow {
		t.Errorf("cross-tenant subject = %s, want allow", got)
	}

	if got := (OwnerPolicy{}).Evaluate(NewSubject(testID(9), testID(2), RoleMember), resource); got != Allow {
		t.Errorf("owner = %s, want allow", got)
	}
	if got := (OwnerPolicy{}).Evaluate(NewSubject(testID(8), testID(2), RoleMember), resource); got != Conceal {
		t.Errorf("non-owner = %s, want conceal", got)
	}
}

func TestPolicySet_DenyOverridesAndConceal(t *testing.T) {
	resource := Resource{TenantID: testID(1), OwnerID: testID(1)}
	allow := PolicyFunc(func(Subject, Resource) Decision { return Allow })
	deny := PolicyFunc(func(Subject, Resource) Decision { return Deny })
	conceal := PolicyFunc(func(Subject, Resource) Decision { return Conceal })

	if got := NewPolicySet(allow, allow).Evaluate(NewSubject(testID(1), testID(1), RoleMember), resource); got != Allow {
		t.Errorf("allow set = %s, want allow", got)
	}
	if got := NewPolicySet(allow, deny).Evaluate(NewSubject(testID(1), testID(1), RoleMember), resource); got != Deny {
		t.Errorf("deny set = %s, want deny (deny overrides)", got)
	}
	if got := NewPolicySet(allow, conceal).Evaluate(NewSubject(testID(1), testID(1), RoleMember), resource); got != Conceal {
		t.Errorf("conceal set = %s, want conceal", got)
	}
	if got := NewPolicySet().Evaluate(NewSubject(testID(1), testID(1), RoleMember), resource); got != Deny {
		t.Errorf("empty set = %s, want deny", got)
	}
}

func TestAuthorize_MapsDecisions(t *testing.T) {
	resource := Resource{TenantID: testID(1), OwnerID: testID(1)}

	t.Run("missing subject is forbidden", func(t *testing.T) {
		err := testAuthorizer().Authorize(context.Background(), testPermissionRead, resource, TenantPolicy{})
		requirePermissionDenied(t, err)
	})

	t.Run("rbac denial is forbidden before policies", func(t *testing.T) {
		ctx := WithSubject(context.Background(), NewSubjectWithPermissions(testID(1), testID(1), RoleMember))
		err := testAuthorizer().Authorize(ctx, PermissionAdminUsersRead, resource, TenantPolicy{})
		requirePermissionDenied(t, err)
	})

	t.Run("policy deny is forbidden", func(t *testing.T) {
		subject := NewSubjectWithPermissions(testID(1), testID(1), RoleMember, testPermissionRead)
		err := testAuthorizer().AuthorizeSubject(subject, testPermissionRead, resource,
			PolicyFunc(func(Subject, Resource) Decision { return Deny }))
		requirePermissionDenied(t, err)
	})

	t.Run("policy conceal is not found", func(t *testing.T) {
		subject := NewSubjectWithPermissions(testID(2), testID(2), RoleMember, testPermissionRead)
		err := testAuthorizer().AuthorizeSubject(subject, testPermissionRead, resource, TenantPolicy{}, OwnerPolicy{})
		requireNotFound(t, err)
	})

	t.Run("allow returns nil", func(t *testing.T) {
		ctx := WithSubject(context.Background(), NewSubjectWithPermissions(testID(1), testID(1), RoleMember, testPermissionRead))
		if err := testAuthorizer().Authorize(ctx, testPermissionRead, resource, TenantPolicy{}, OwnerPolicy{}); err != nil {
			t.Fatalf("authorize: %v", err)
		}
	})
}

func TestAuthorize_SystemContext(t *testing.T) {
	resource := Resource{TenantID: testID(1), OwnerID: testID(1)}

	t.Run("no subject and no marker is denied", func(t *testing.T) {
		if IsSystemContext(context.Background()) {
			t.Fatal("a plain context must not be a system context")
		}
		err := testAuthorizer().Authorize(context.Background(), PermissionAdminUsersRead, resource, TenantPolicy{}, OwnerPolicy{})
		requirePermissionDenied(t, err)
	})

	t.Run("system marker without subject is allowed", func(t *testing.T) {
		ctx := SystemContext(context.Background())
		if err := testAuthorizer().Authorize(ctx, PermissionAdminUsersRead, resource, TenantPolicy{}, OwnerPolicy{}); err != nil {
			t.Fatalf("authorize system context: %v", err)
		}
	})

	t.Run("subject wins over the system marker", func(t *testing.T) {
		ctx := SystemContext(WithSubject(context.Background(), NewSubjectWithPermissions(testID(2), testID(2), RoleMember, testPermissionRead)))
		requirePermissionDenied(t, testAuthorizer().Authorize(ctx, PermissionAdminUsersRead, resource, TenantPolicy{}))
		requireNotFound(t, testAuthorizer().Authorize(ctx, testPermissionRead, resource, TenantPolicy{}, OwnerPolicy{}))
	})
}

func requirePermissionDenied(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(apperror.CodePermissionDenied) || statusOf(appErr) != 403 {
		t.Fatalf("error = %v, want 403 permission.denied", err)
	}
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || statusOf(appErr) != 404 {
		t.Fatalf("error = %v, want 404 not found", err)
	}
}

// testID returns a deterministic UUID for a small fixture number.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

func statusOf(appErr *apperror.AppError) int {
	_, spec := apperror.PlatformCatalog().Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
