package authz

import (
	"context"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Decision is the outcome of a policy evaluation.
type Decision uint8

const (
	// Allow grants access.
	Allow Decision = iota
	// Deny rejects access with 403 (permission.denied).
	Deny
	// Conceal rejects access with 404 so the caller cannot learn whether the
	// resource exists in another tenant or belongs to another user.
	Conceal
)

// DecisionAllow, DecisionDeny, and DecisionConceal are aliases for callers
// that prefer the prefixed spelling.
const (
	DecisionAllow   = Allow
	DecisionDeny    = Deny
	DecisionConceal = Conceal
)

// String renders a decision for logs and tests.
func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	case Conceal:
		return "conceal"
	default:
		return "unknown"
	}
}

// Resource is the object a policy evaluates. TenantID and OwnerID are the
// only facts the built-in policies need; modules may use additional subject
// attributes for their own policies.
type Resource struct {
	TenantID uuid.UUID
	OwnerID  uuid.UUID
}

// Policy evaluates a subject against a resource.
type Policy interface {
	Evaluate(subject Subject, resource Resource) Decision
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(subject Subject, resource Resource) Decision

// Evaluate implements Policy.
func (f PolicyFunc) Evaluate(subject Subject, resource Resource) Decision {
	return f(subject, resource)
}

// PolicySet combines policies. Evaluation is deny-overrides: any Deny makes
// the set deny even if another policy allows. Conceal is preserved when no
// policy denies, so cross-tenant/owner checks map to 404. An empty set denies
// (fail closed).
type PolicySet []Policy

// NewPolicySet builds a PolicySet from the supplied policies.
func NewPolicySet(policies ...Policy) PolicySet {
	return PolicySet(policies)
}

// Evaluate implements Policy.
func (p PolicySet) Evaluate(subject Subject, resource Resource) Decision {
	if len(p) == 0 {
		return Deny
	}
	decision := Allow
	for _, policy := range p {
		if policy == nil {
			return Deny
		}
		switch policy.Evaluate(subject, resource) {
		case Deny:
			// Deny overrides every other outcome, including a later conceal.
			return Deny
		case Conceal:
			decision = Conceal
		}
	}
	return decision
}

// AllowPolicy allows every resource. Use it when the permission check alone
// is the decision (for example a process-wide platform capability).
type AllowPolicy struct{}

// Evaluate implements Policy.
func (AllowPolicy) Evaluate(Subject, Resource) Decision { return Allow }

// TenantPolicy allows access only within the subject's tenant. A
// platform-level subject with CrossTenant set may pass. A mismatch is concealed
// (404) so cross-tenant resource ids do not reveal existence.
type TenantPolicy struct{}

// Evaluate implements Policy.
func (TenantPolicy) Evaluate(subject Subject, resource Resource) Decision {
	if subject.CrossTenant || subject.TenantID == resource.TenantID {
		return Allow
	}
	return Conceal
}

// OwnerPolicy allows access only to the resource owner. A mismatch is
// concealed (404). Callers normally combine it with TenantPolicy so both
// boundaries are checked.
type OwnerPolicy struct{}

// Evaluate implements Policy.
func (OwnerPolicy) Evaluate(subject Subject, resource Resource) Decision {
	if subject.UserID == resource.OwnerID {
		return Allow
	}
	return Conceal
}

// Authorizer is the single service-level authorization path, bound to the
// injected permission catalog. Services receive it in their dependencies;
// there is no package-level authorization entry point.
type Authorizer struct {
	catalog *Catalog
}

// NewAuthorizer binds the authorizer to a catalog. A nil catalog fails closed
// for subject-bearing contexts (unknown permission) and still allows system
// contexts.
func NewAuthorizer(catalog *Catalog) Authorizer {
	return Authorizer{catalog: catalog}
}

// Has reports whether role is granted permission p in the bound catalog.
func (a Authorizer) Has(role string, permission Permission) bool {
	return a.catalog != nil && a.catalog.Has(role, permission)
}

// IsKnownPermission reports whether permission is part of the bound catalog.
func (a Authorizer) IsKnownPermission(permission Permission) bool {
	return a.catalog != nil && a.catalog.IsKnownPermission(permission)
}

// Authorize is the single service-level authorization path. With a subject in
// ctx (installed by JWTAuth or the API-key middleware through WithSubject) it
// performs an RBAC pre-check followed by policy evaluation: Deny maps to 403
// permission.denied and Conceal to 404 not found. A subject always wins over
// the system marker, so a request context can never escalate by also carrying
// it. Without a subject, a context marked by SystemContext is allowed; any
// other context fails closed with 403.
func (a Authorizer) Authorize(ctx context.Context, permission Permission, resource Resource, policies ...Policy) error {
	if subject, ok := SubjectFromContext(ctx); ok {
		return a.AuthorizeSubject(subject, permission, resource, policies...)
	}
	if IsSystemContext(ctx) {
		return nil
	}
	return permissionDenied()
}

// AuthorizeSubject is Authorize for callers that already hold a Subject. A
// permission outside the injected catalog is denied (a typo must never grant
// access), mirroring the fail-closed route middleware.
func (a Authorizer) AuthorizeSubject(subject Subject, permission Permission, resource Resource, policies ...Policy) error {
	if !a.IsKnownPermission(permission) || !subject.Has(permission) {
		return permissionDenied()
	}
	switch PolicySet(policies).Evaluate(subject, resource) {
	case Allow:
		return nil
	case Conceal:
		return apperror.NewNotFound("resource not found")
	default:
		return permissionDenied()
	}
}

func permissionDenied() error {
	return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
}
