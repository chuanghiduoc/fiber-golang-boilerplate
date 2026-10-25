package authz

import "context"

// Attribute keys used by the built-in policies and middleware. Attributes are
// intentionally plain strings so modules can attach their own facts without
// changing the Subject contract.
const (
	AttributeAuthMethod = "auth_method"
	AttributeAPIKeyID   = "api_key_id"
)

// Subject is the authenticated caller as seen by authorization decisions.
//
// Permissions is an explicit permission set. A nil map means "derive from
// Role" (normal JWT sessions). A non-nil map is authoritative and may be empty,
// which grants no permissions. API keys use a non-nil map populated from their
// scopes, so a key can never exceed the scopes recorded at creation time.
//
// CrossTenant allows a platform-level caller to pass TenantPolicy. It defaults
// to false and is never set from JWT or API-key middleware.
type Subject struct {
	UserID      int64
	TenantID    int64
	Role        string
	Permissions map[Permission]struct{}
	CrossTenant bool
	Attributes  map[string]string
}

// NewSubject builds a subject whose permissions derive from role.
func NewSubject(userID, tenantID int64, role string) Subject {
	return Subject{UserID: userID, TenantID: tenantID, Role: role}
}

// NewSubjectWithPermissions builds a subject with an explicit, authoritative
// permission set. An empty list yields a non-nil empty map (deny everything),
// which is different from NewSubject's role-derived behavior.
func NewSubjectWithPermissions(userID, tenantID int64, role string, permissions ...Permission) Subject {
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

// Has reports whether the subject is granted permission. Explicit permissions
// take precedence over the role catalog.
func (s Subject) Has(permission Permission) bool {
	if s.Permissions != nil {
		_, ok := s.Permissions[permission]
		return ok
	}
	return Has(s.Role, permission)
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
