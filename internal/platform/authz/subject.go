package authz

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Attribute keys used by the built-in policies and middleware. Attributes are
// intentionally plain strings so modules can attach their own facts without
// changing the Subject contract.
const (
	AttributeAuthMethod = "auth_method"
	AttributeAPIKeyID   = "api_key_id"
)

// Subject is the authenticated caller as seen by authorization decisions.
//
// Permissions is the effective permission set, resolved at authentication
// time. It is authoritative: a nil or empty map grants nothing (fail closed),
// and a role name never grants permissions by itself. JWT sessions carry the
// set loaded by the session store; API keys carry their scopes intersected
// with the owner's effective permissions.
//
// CrossTenant allows a platform-level caller to pass TenantPolicy. It is set
// for a subject holding a platform role assignment and never for a plain
// tenant member.
type Subject struct {
	UserID      uuid.UUID
	TenantID    uuid.UUID
	Role        string
	Permissions map[Permission]struct{}
	CrossTenant bool
	Attributes  map[string]string
	// AMR is the access token's authentication-methods set (pwd, google,
	// otp, rc). Tokens without it have an empty set.
	AMR []string
	// MFAVerifiedAt is the session's second-factor freshness (nil-safe zero
	// time means never verified). RequireMFA compares it with MFA_MAX_AGE.
	MFAVerifiedAt time.Time
}

// NewSubject builds an identity-only subject with no permissions. It is for
// tests and diagnostics; request paths always carry the effective permission
// set from the session store or the API key.
func NewSubject(userID, tenantID uuid.UUID, role string) Subject {
	return Subject{UserID: userID, TenantID: tenantID, Role: role}
}

// NewSubjectWithPermissions builds a subject with an explicit, authoritative
// permission set. An empty list yields a non-nil empty map (deny everything),
// which is different from NewSubject's role-derived behavior.
func NewSubjectWithPermissions(userID, tenantID uuid.UUID, role string, permissions ...Permission) Subject {
	granted := make(map[Permission]struct{}, len(permissions))
	for _, permission := range permissions {
		granted[permission] = struct{}{}
	}
	return Subject{
		UserID:      userID,
		TenantID:    tenantID,
		Role:        role,
		Permissions: granted,
	}
}

// Has reports whether the subject is granted permission. Only the explicit
// permission set counts: a role name never grants anything on its own, because
// the effective set is resolved once by the session store (or the API-key
// scope intersection) and a missing set must fail closed.
func (s Subject) Has(permission Permission) bool {
	_, ok := s.Permissions[permission]
	return ok
}

// Attribute returns an attribute value or "".
func (s Subject) Attribute(name string) string {
	if s.Attributes == nil {
		return ""
	}
	return s.Attributes[name]
}

// WithAttribute returns a copy with one attribute set. The returned subject
// shares no map with the receiver, so callers cannot mutate shared state.
func (s Subject) WithAttribute(name, value string) Subject {
	attrs := make(map[string]string, len(s.Attributes)+1)
	for k, v := range s.Attributes {
		attrs[k] = v
	}
	attrs[name] = value
	s.Attributes = attrs
	return s
}

// WithAMR returns a copy carrying the access token's authentication methods.
func (s Subject) WithAMR(amr []string) Subject {
	s.AMR = append([]string(nil), amr...)
	return s
}

// WithMFAVerifiedAt returns a copy carrying the session's second-factor
// freshness timestamp.
func (s Subject) WithMFAVerifiedAt(at time.Time) Subject {
	s.MFAVerifiedAt = at
	return s
}

// HasMFA reports whether the subject authenticated with an MFA method: a
// TOTP code (otp), a recovery code (rc), or a passkey (hwk/swk).
func (s Subject) HasMFA() bool {
	for _, method := range s.AMR {
		if method == "otp" || method == "rc" || method == "hwk" || method == "swk" {
			return true
		}
	}
	return false
}

// HasPasskey reports whether the subject authenticated with a phishing-resistant
// passkey method (hwk or swk, RFC 8176).
func (s Subject) HasPasskey() bool {
	for _, method := range s.AMR {
		if method == "hwk" || method == "swk" {
			return true
		}
	}
	return false
}

type subjectContextKey struct{}

// WithSubject stores the authorization subject in a request context.
func WithSubject(ctx context.Context, subject Subject) context.Context {
	return context.WithValue(ctx, subjectContextKey{}, subject)
}

// SubjectFromContext returns the subject stored by WithSubject.
func SubjectFromContext(ctx context.Context) (Subject, bool) {
	subject, ok := ctx.Value(subjectContextKey{}).(Subject)
	return subject, ok
}

type systemContextKey struct{}

// SystemContext marks ctx as a trusted system caller: a durable job handler,
// the scheduler, the seeder, or another in-process worker acting for the
// platform rather than for a user. Authorize allows such a context when it
// carries no subject; with neither a subject nor this marker it denies (fail
// closed).
//
// Never derive a system context from an HTTP request: it skips every policy.
// Request handling authorizes through the subject installed by
// authentication middleware.
func SystemContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemContextKey{}, true)
}

// IsSystemContext reports whether ctx was marked by SystemContext.
func IsSystemContext(ctx context.Context) bool {
	marked, _ := ctx.Value(systemContextKey{}).(bool)
	return marked
}
