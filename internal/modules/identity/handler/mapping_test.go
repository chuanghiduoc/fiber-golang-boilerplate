package handler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
)

func TestToUserResponse_MapsMembership(t *testing.T) {
	joined := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	removed := joined.Add(time.Hour)
	resp := toUserResponse(&core.Member{
		User: core.User{ID: testID(7), Email: "m@example.com", Name: "M", Status: core.UserStatusActive, HasPassword: true,
			CreatedAt: joined.Add(-time.Hour), UpdatedAt: joined},
		Membership: core.Membership{TenantID: testID(3), UserID: testID(7), Role: core.RoleAdmin, Status: core.MembershipStatusRemoved,
			RemovedAt: &removed, CreatedAt: joined, UpdatedAt: removed},
	})
	assert.Equal(t, testID(3), resp.TenantID)
	assert.Equal(t, core.RoleAdmin, resp.Role)
	assert.Equal(t, "removed", resp.MembershipStatus)
	assert.Equal(t, joined, resp.JoinedAt)
	assert.Equal(t, removed, resp.UpdatedAt, "the later of the user/membership updates")
	require.NotNil(t, resp.DeletedAt)
	assert.Equal(t, removed, *resp.DeletedAt)
	assert.False(t, resp.EmailVerified)
}
