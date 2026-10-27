// Package sessiontest provides session stores for tests that need JWT
// authentication without a database. It exists so module handler tests can
// wire authentication without importing the platform session implementation
// (a module boundary the architecture tests enforce).
package sessiontest

import (
	"context"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
)

type allowAll struct{}

// AllowAll returns a store that accepts every token as an active member of an
// operational tenant, with no permissions. Use it where the test exercises
// handler behavior, not authorization or session lifecycle.
func AllowAll() session.Store { return allowAll{} }

func (allowAll) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{Active: true, TenantActive: true}, nil
}

func (allowAll) Invalidate(context.Context, uuid.UUID) error { return nil }
func (allowAll) InvalidatePermissions(context.Context) error { return nil }
