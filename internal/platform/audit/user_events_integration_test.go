//go:build integration

package audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TestUserEventReaderTargetsTheMemberNotTheActor covers the admin erasure
// status read: the admin is the actor and the member is the resource, so the
// target query must find a row the actor query does not.
func TestUserEventReaderTargetsTheMemberNotTheActor(t *testing.T) {
	fx := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	recorder := NewPostgresRecorder(fx.Scoped())
	admin, member := testID(901), testID(902)
	require.NoError(t, recorder.Record(ctx, Event{
		ActorID:    admin,
		Action:     "admin.user.erasure_requested",
		Resource:   "user",
		ResourceID: member.String(),
		RequestID:  fmt.Sprintf("audit-target-%d", time.Now().UnixNano()),
	}))

	reader := NewUserEventReader(fx.Scoped())
	about, found, err := reader.LatestUserTargetEvent(ctx, tenant.DefaultID, member, "admin.user.erasure_requested")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "admin.user.erasure_requested", about.Action)
	assert.Equal(t, "user", about.Resource)
	assert.Equal(t, member.String(), about.ResourceID)

	mine, err := reader.ListUserEvents(ctx, tenant.DefaultID, member, 10)
	require.NoError(t, err)
	assert.Empty(t, mine.Events, "the member did not act on their own erasure")

	actor, err := reader.ListUserEvents(ctx, tenant.DefaultID, admin, 10)
	require.NoError(t, err)
	require.Len(t, actor.Events, 1)

	require.NoError(t, recorder.Record(ctx, Event{
		ActorID:    admin,
		Action:     "admin.user.roles_changed",
		Resource:   "user",
		ResourceID: member.String(),
		RequestID:  fmt.Sprintf("audit-target-limit-%d", time.Now().UnixNano()),
	}))
	page, err := reader.ListUserEvents(ctx, tenant.DefaultID, admin, 1)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	assert.True(t, page.Truncated, "limit+1 detection must signal older rows")

	// A newer, unrelated event on the same member must not hide the erasure
	// request: the lookup filters by action instead of scanning a page.
	stillFound, ok, err := reader.LatestUserTargetEvent(ctx, tenant.DefaultID, member, "admin.user.erasure_requested")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "admin.user.erasure_requested", stillFound.Action)
}
