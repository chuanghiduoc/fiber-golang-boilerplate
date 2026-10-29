// Package authzhttp is the Fiber adapter for internal/platform/authz: route
// permission middleware (Require/RequireAny) and the Fiber-locals subject
// carrier used by authentication middleware. Services authorize through
// authz.Authorize on the request context and never import this package.
package authzhttp

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// SetSubject stores the subject in the request context, which route
// middleware and handlers read through SubjectFromFiber.
func SetSubject(c fiber.Ctx, subject authz.Subject) {
	c.SetContext(authz.WithSubject(c.Context(), subject))
}

// SubjectFromFiber returns the subject stored by SetSubject.
func SubjectFromFiber(c fiber.Ctx) (authz.Subject, bool) {
	subject, ok := authz.SubjectFromContext(c.Context())
	if !ok {
		return authz.Subject{}, false
	}
	// A subject is only meaningful with a role or an explicit permission set;
	// reject the zero value so a missing local cannot grant access through an
	// unknown role.
	if subject.Role == "" && subject.Permissions == nil {
		return authz.Subject{}, false
	}
	return subject, true
}

// requestSubject returns the subject installed by authentication middleware.
// There is no role fallback: a bare "role" local from older or third-party
// middleware never grants a permission.
func requestSubject(c fiber.Ctx) (authz.Subject, bool) {
	return SubjectFromFiber(c)
}

// Require returns middleware that allows the request only when the
// authenticated subject grants permission. It must run after authentication.
// API-key subjects use their explicit scopes; JWT subjects carry the session's
// effective permission set. A missing subject or a denied permission yields a
// 403 problem response.
func Require(permission authz.Permission) fiber.Handler {
	return func(c fiber.Ctx) error {
		subject, ok := requestSubject(c)
		if !ok || !subject.Has(permission) {
			return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
		}
		return c.Next()
	}
}

// RequireAny returns middleware that allows the request when the authenticated
// subject grants at least one of the supplied permissions. An empty list denies
// every request. It must run after authentication.
func RequireAny(permissions ...authz.Permission) fiber.Handler {
	return func(c fiber.Ctx) error {
		subject, ok := requestSubject(c)
		if !ok {
			return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
		}
		for _, permission := range permissions {
			if subject.Has(permission) {
				return c.Next()
			}
		}
		return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
	}
}

// RequireMFA returns middleware that allows the request only when the
// authenticated subject's access token carries an MFA method (otp, rc, hwk, or
// swk) and the session's second factor is no older than maxAge (NIST SP 800-63B
// rev4 §2.2.3: a second factor verified weeks ago is not AAL2 anymore). A
// missing or stale proof is refused with 403 auth.mfa_required; the client
// refreshes it with POST /me/mfa/step-up. It must run after authentication.
// API-key subjects have no amr and are denied. A non-positive maxAge still
// requires a recorded verification, just without an age bound.
func RequireMFA(maxAge time.Duration) fiber.Handler {
	return requireMFAMethod(maxAge, false)
}

// RequirePasskeyMFA is the phishing-resistant variant of RequireMFA: it
// accepts only a passkey (hwk or swk), never a TOTP or recovery code. Every
// platform:* permission uses it automatically, so a platform administrator
// cannot reach cross-tenant operations with a phishable factor.
func RequirePasskeyMFA(maxAge time.Duration) fiber.Handler {
	return requireMFAMethod(maxAge, true)
}

func requireMFAMethod(maxAge time.Duration, passkeyOnly bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		subject, ok := requestSubject(c)
		if !ok {
			return apperror.NewCode(apperror.CodeMFARequired, "multi-factor authentication is required")
		}
		if passkeyOnly && !subject.HasPasskey() {
			return apperror.NewCode(apperror.CodeMFARequired,
				"a passkey is required for this operation; register one at POST /me/mfa/passkeys/options")
		}
		if !subject.HasMFA() {
			return apperror.NewCode(apperror.CodeMFARequired, "multi-factor authentication is required")
		}
		now := time.Now()
		if subject.MFAVerifiedAt.IsZero() || subject.MFAVerifiedAt.After(now) ||
			(maxAge > 0 && now.Sub(subject.MFAVerifiedAt) > maxAge) {
			return apperror.NewCode(apperror.CodeMFARequired,
				"multi-factor authentication has expired; re-authenticate at POST /me/mfa/step-up")
		}
		return c.Next()
	}
}
