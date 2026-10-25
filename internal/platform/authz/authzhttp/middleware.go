// Package authzhttp is the Fiber adapter for internal/platform/authz: route
// permission middleware (Require/RequireAny) and the Fiber-locals subject
// carrier used by authentication middleware. Services authorize through
// authz.Authorize on the request context and never import this package.
package authzhttp

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// roleLocalKey is the Fiber locals key populated by the JWTAuth middleware.
const roleLocalKey = "role"

// subjectLocalKey is the Fiber locals key used by the authentication
// middleware. Keeping the subject in Fiber locals lets route middleware make a
// decision without re-reading the context, while SetSubject also updates the
// context for services and repositories.
const subjectLocalKey = "authz_subject"

// SetSubject stores the subject in Fiber locals and the request context.
func SetSubject(c fiber.Ctx, subject authz.Subject) {
	fiber.Locals[authz.Subject](c, subjectLocalKey, subject)
	c.SetContext(authz.WithSubject(c.Context(), subject))
}

// SubjectFromFiber returns the subject stored by SetSubject.
func SubjectFromFiber(c fiber.Ctx) (authz.Subject, bool) {
	subject := fiber.Locals[authz.Subject](c, subjectLocalKey)
	// A subject is only meaningful with either a role or an explicit
	// permission set; reject the zero value so a missing local cannot grant
	// access through an unknown role.
	if subject.Role == "" && subject.Permissions == nil {
		return authz.Subject{}, false
	}
	return subject, true
}

// requestSubject returns the subject installed by authentication middleware.
// When only a role local is present (older tests or third-party middleware),
// it derives permissions from the role catalog so route behavior is unchanged.
func requestSubject(c fiber.Ctx) (authz.Subject, bool) {
	if subject, ok := SubjectFromFiber(c); ok {
		return subject, true
	}
	role := fiber.Locals[string](c, roleLocalKey)
	if role == "" {
		return authz.Subject{}, false
	}
	return authz.NewSubject(0, 0, role), true
}

// Require returns middleware that allows the request only when the
// authenticated subject grants permission. It must run after authentication.
// API-key subjects use their explicit scopes; JWT subjects derive permissions
// from their role. A missing subject or a denied permission yields a 403
// problem response.
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
