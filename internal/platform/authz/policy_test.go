package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestSubject_Permissions(t *testing.T) {
	roleSubject := NewSubject(10, 1, RoleUser)
	if !roleSubject.Has(PermissionFilesRead) {
		t.Fatal("role subject should derive files:read from the user role")
	}
	if roleSubject.Has(PermissionAdminUsersRead) {
		t.Fatal("role subject must not gain admin permissions")
	}

	scoped := NewSubjectWithPermissions(10, 1, RoleAdmin, PermissionFilesRead)
	if !scoped.Has(PermissionFilesRead) {
		t.Fatal("explicit scope should be granted")
	}
	if scoped.Has(PermissionAdminUsersRead) {
		t.Fatal("explicit scopes must override the admin role catalog")
	}

	empty := NewSubjectWithPermissions(10, 1, RoleAdmin)
	if empty.Permissions == nil {
		t.Fatal("explicit empty scopes must be non-nil")
	}
	if empty.Has(PermissionAdminUsersRead) || empty.Has(PermissionFilesRead) {
		t.Fatal("explicit empty scopes must deny everything")
	}
}

func TestSubject_AttributesAreCopied(t *testing.T) {
	base := NewSubject(1, 2, RoleUser)
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
	subject := NewSubjectWithPermissions(7, 3, RoleUser, PermissionFilesRead)
	ctx = WithSubject(ctx, subject)
	got, ok := SubjectFromContext(ctx)
	if !ok {
		t.Fatal("subject missing from context")
	}
	if got.UserID != 7 || got.TenantID != 3 || !got.Has(PermissionFilesRead) {
		t.Fatalf("subject = %+v, want user 7 tenant 3 files:read", got)
	}
}

func TestPolicies(t *testing.T) {
	resource := Resource{TenantID: 2, OwnerID: 9}

	if got := (TenantPolicy{}).Evaluate(NewSubject(1, 2, RoleUser), resource); got != Allow {
		t.Errorf("same tenant = %s, want allow", got)
	}
	if got := (TenantPolicy{}).Evaluate(NewSubject(1, 3, RoleUser), resource); got != Conceal {
		t.Errorf("cross tenant = %s, want conceal", got)
	}
	cross := NewSubject(1, 3, RoleAdmin)
	cross.CrossTenant = true
	if got := (TenantPolicy{}).Evaluate(cross, resource); got != Allow {
		t.Errorf("cross-tenant subject = %s, want allow", got)
	}

	if got := (OwnerPolicy{}).Evaluate(NewSubject(9, 2, RoleUser), resource); got != Allow {
		t.Errorf("owner = %s, want allow", got)
	}
	if got := (OwnerPolicy{}).Evaluate(NewSubject(8, 2, RoleUser), resource); got != Conceal {
		t.Errorf("non-owner = %s, want conceal", got)
	}
}

func TestPolicySet_DenyOverridesAndConceal(t *testing.T) {
	resource := Resource{TenantID: 1, OwnerID: 1}
	allow := PolicyFunc(func(Subject, Resource) Decision { return Allow })
	deny := PolicyFunc(func(Subject, Resource) Decision { return Deny })
	conceal := PolicyFunc(func(Subject, Resource) Decision { return Conceal })

	if got := NewPolicySet(allow, allow).Evaluate(NewSubject(1, 1, RoleUser), resource); got != Allow {
		t.Errorf("allow set = %s, want allow", got)
	}
	if got := NewPolicySet(allow, deny).Evaluate(NewSubject(1, 1, RoleUser), resource); got != Deny {
		t.Errorf("deny set = %s, want deny (deny overrides)", got)
	}
	if got := NewPolicySet(allow, conceal).Evaluate(NewSubject(1, 1, RoleUser), resource); got != Conceal {
		t.Errorf("conceal set = %s, want conceal", got)
	}
	if got := NewPolicySet().Evaluate(NewSubject(1, 1, RoleUser), resource); got != Deny {
		t.Errorf("empty set = %s, want deny", got)
	}
}

func TestAuthorize_MapsDecisions(t *testing.T) {
	resource := Resource{TenantID: 1, OwnerID: 1}

	t.Run("missing subject is forbidden", func(t *testing.T) {
		err := Authorize(context.Background(), PermissionFilesRead, resource, TenantPolicy{})
		requirePermissionDenied(t, err)
	})

	t.Run("rbac denial is forbidden before policies", func(t *testing.T) {
		ctx := WithSubject(context.Background(), NewSubject(1, 1, RoleUser))
		err := Authorize(ctx, PermissionAdminUsersRead, resource, TenantPolicy{})
		requirePermissionDenied(t, err)
	})

	t.Run("policy deny is forbidden", func(t *testing.T) {
		subject := NewSubject(1, 1, RoleUser)
		err := AuthorizeSubject(subject, PermissionFilesRead, resource,
			PolicyFunc(func(Subject, Resource) Decision { return Deny }))
		requirePermissionDenied(t, err)
	})

	t.Run("policy conceal is not found", func(t *testing.T) {
		subject := NewSubject(2, 2, RoleUser)
		err := AuthorizeSubject(subject, PermissionFilesRead, resource, TenantPolicy{}, OwnerPolicy{})
		requireNotFound(t, err)
	})

	t.Run("allow returns nil", func(t *testing.T) {
		ctx := WithSubject(context.Background(), NewSubject(1, 1, RoleUser))
		if err := Authorize(ctx, PermissionFilesRead, resource, TenantPolicy{}, OwnerPolicy{}); err != nil {
			t.Fatalf("authorize: %v", err)
		}
	})
}

func requirePermissionDenied(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 403 || appErr.Code != string(apperror.CodePermissionDenied) {
		t.Fatalf("error = %v, want 403 permission.denied", err)
	}
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 404 {
		t.Fatalf("error = %v, want 404 not found", err)
	}
}
