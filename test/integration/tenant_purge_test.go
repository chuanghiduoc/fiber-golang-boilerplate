//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func createTrialTenant(t *testing.T, ctx context.Context, h *privacyHarness, slug string, trialEnd *time.Time) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name, status, trial_ends_at) VALUES ($1, $1, 'trialing', $2) RETURNING id`,
		slug, trialEnd).Scan(&id))
	_, err := h.pool.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`, id)
	require.NoError(t, err)
	return id
}

// seedTenantCheckFixtures adds every kind of tenant-wide row the purge must
// remove: a custom role and assignment, an invitation, a tenant-wide feature
// target, a webhook inbound source/event, and an audit row that must survive.
func seedTenantCheckFixtures(t *testing.T, h *privacyHarness, tenantID, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var roleID uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO roles (tenant_id, scope, key, name) VALUES ($1, 'tenant', 'analyst', 'Analyst') RETURNING id`,
		tenantID).Scan(&roleID))
	_, err := h.pool.Exec(ctx,
		`INSERT INTO membership_roles (tenant_id, user_id, role_id) VALUES ($1, $2, $3)`,
		tenantID, userID, roleID)
	require.NoError(t, err)

	_, err = h.pool.Exec(ctx,
		`INSERT INTO invitations (tenant_id, email_normalized, role_id, token_hash, invited_by, expires_at)
		 VALUES ($1, $2, (SELECT id FROM roles WHERE tenant_id IS NULL AND scope = 'tenant' AND key = 'member'), sha256($3::bytea), $4, NOW() + interval '7 days')`,
		tenantID, fmt.Sprintf("invitee-%s-%s@example.test", tenantID, userID),
		fmt.Sprintf("invite-token-%s-%s", tenantID, userID), userID)
	require.NoError(t, err)

	_, err = h.pool.Exec(ctx,
		`INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled)
		 VALUES ('privacy_test_flag', $1, NULL, false)`, tenantID)
	require.NoError(t, err)

	var sourceID uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO webhook_inbound_sources (tenant_id, name, slug, status)
		 VALUES ($1, 'inbound', $2, 'active') RETURNING id`,
		tenantID, fmt.Sprintf("inbound-%s", tenantID)).Scan(&sourceID))
	_, err = h.pool.Exec(ctx,
		`INSERT INTO webhook_inbound_source_secrets (tenant_id, source_id, version, secret_ciphertext)
		 VALUES ($1, $2, 1, 'sealed')`,
		tenantID, sourceID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO webhook_inbound_events (tenant_id, source_id, external_event_id, signature, payload)
		 VALUES ($1, $2, $3, 'sig', '{}'::jsonb)`,
		tenantID, sourceID, fmt.Sprintf("event-%s", tenantID))
	require.NoError(t, err)

	_, err = h.pool.Exec(ctx,
		`INSERT INTO audit_events (tenant_id, actor_id, actor_type, action, resource, resource_id)
		 VALUES ($1, $2, 'user', 'files.uploaded', 'file', '1')`, tenantID, userID)
	require.NoError(t, err)
}

func zeroForTenant(t *testing.T, h *privacyHarness, table string, tenantID uuid.UUID) {
	t.Helper()
	var count int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenantID).Scan(&count),
		"count %s for tenant %s", table, tenantID)
	assert.Zero(t, count, "%s still holds rows for tenant %s", table, tenantID)
}

func countForTenant(t *testing.T, h *privacyHarness, table string, tenantID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenantID).Scan(&count))
	return count
}

func TestTenantPurge_DeletedTenantIsErasedAndIsIdempotent(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	tenantA := testutil.CreateTenant(t, ctx, h.pool, "purge-a")
	tenantB := testutil.CreateTenant(t, ctx, h.pool, "purge-b")
	memberA := testutil.CreateMember(t, ctx, h.pool, tenantA, "purge-a@example.com", testutil.MemberOptions{Role: "owner"})
	memberB := testutil.CreateMember(t, ctx, h.pool, tenantB, "purge-b@example.com", testutil.MemberOptions{Role: "owner"})
	// A removed membership is still part of the tenant's data and must be
	// erased (with any export its row still owns) before the final delete.
	removedA := testutil.CreateMember(t, ctx, h.pool, tenantA, "purge-removed@example.com", testutil.MemberOptions{})
	_, err := h.pool.Exec(ctx,
		`UPDATE memberships SET status = 'removed', removed_at = NOW() WHERE tenant_id = $1 AND user_id = $2`,
		tenantA, removedA.UserID)
	require.NoError(t, err)

	h.seed(t, tenantA, memberA.UserID)
	h.seed(t, tenantB, memberB.UserID)
	seedTenantCheckFixtures(t, h, tenantA, memberA.UserID)
	seedTenantCheckFixtures(t, h, tenantB, memberB.UserID)

	removedExportPath := fmt.Sprintf("%s/%s/exports/removed.json", tenantA, removedA.UserID)
	require.NoError(t, h.store.Put(ctx, removedExportPath, strings.NewReader(`{"userId":1}`), 12, "application/json"))
	_, err = h.pool.Exec(ctx,
		`INSERT INTO data_exports (tenant_id, user_id, status, storage_path, expires_at)
		 VALUES ($1, $2, 'ready', $3, NOW() + interval '7 days')`, tenantA, removedA.UserID, removedExportPath)
	require.NoError(t, err)

	auditBefore := countForTenant(t, h, "audit_events", tenantA)
	filesBeforeA := countForTenant(t, h, "files", tenantA)
	require.NotZero(t, filesBeforeA)
	filesBeforeB := countForTenant(t, h, "files", tenantB)

	_, err = h.pool.Exec(ctx,
		`UPDATE tenants SET status = 'deleted', deletion_scheduled_at = NOW() WHERE id = $1`, tenantA)
	require.NoError(t, err)

	handler := h.jobs.handler(t, "identity.tenants.purge_deleted")
	require.NoError(t, handler(ctx, nil))

	var status string
	var purgedAt, otherPurgedAt *time.Time
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT status, purged_at FROM tenants WHERE id = $1`, tenantA).Scan(&status, &purgedAt))
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT purged_at FROM tenants WHERE id = $1`, tenantB).Scan(&otherPurgedAt))
	assert.Equal(t, "deleted", status, "the tenants row stays")
	require.NotNil(t, purgedAt, "completion is recorded")
	assert.Nil(t, otherPurgedAt, "another tenant is never marked purged")

	for _, table := range []string{
		"files", "notifications", "notification_preferences", "api_keys",
		"feature_flag_targets", "webhook_endpoints", "webhook_events",
		"webhook_deliveries", "webhook_inbound_sources", "webhook_inbound_events",
		"invitations", "data_exports", "sessions", "refresh_tokens", "memberships", "roles",
		"tenant_settings",
	} {
		zeroForTenant(t, h, table, tenantA)
	}
	var removedMemberships int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM memberships WHERE tenant_id = $1`, tenantA).Scan(&removedMemberships))
	assert.Zero(t, removedMemberships)
	_, err = h.store.Get(ctx, h.records[fmt.Sprintf("%s/%s", tenantA, memberA.UserID)])
	assert.ErrorIs(t, err, storage.ErrNotFound, "purge deletes the tenant's file blobs")
	_, err = h.store.Get(ctx, removedExportPath)
	assert.ErrorIs(t, err, storage.ErrNotFound, "purge deletes the tenant's export blobs")

	// The other tenant keeps every row and blob.
	assert.Equal(t, filesBeforeB, countForTenant(t, h, "files", tenantB))
	assert.NotZero(t, countForTenant(t, h, "notifications", tenantB))
	assert.NotZero(t, countForTenant(t, h, "api_keys", tenantB))
	assert.NotZero(t, countForTenant(t, h, "webhook_endpoints", tenantB))
	assert.NotZero(t, countForTenant(t, h, "memberships", tenantB))
	reader, err := h.store.Get(ctx, h.records[fmt.Sprintf("%s/%s", tenantB, memberB.UserID)])
	require.NoError(t, err, "the other tenant's blob stays")
	require.NoError(t, reader.Close())

	// audit_events is append-only: every original row is still there and the
	// purge added its completion event.
	auditAfter := countForTenant(t, h, "audit_events", tenantA)
	assert.Equal(t, auditBefore+1, auditAfter, "only the tenant.purged row is added")
	var original, purged int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'files.uploaded'`, tenantA).Scan(&original))
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.purged'`, tenantA).Scan(&purged))
	assert.Equal(t, int64(1), original, "existing audit rows are never deleted")
	assert.Equal(t, int64(1), purged)

	// At-least-once delivery is a no-op.
	require.NoError(t, handler(ctx, nil))
	var purgedAgain *time.Time
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT purged_at FROM tenants WHERE id = $1`, tenantA).Scan(&purgedAgain))
	require.NotNil(t, purgedAgain)
	assert.True(t, purgedAt.Equal(*purgedAgain), "a retry does not rewrite the completion marker")
	assert.Equal(t, auditAfter, countForTenant(t, h, "audit_events", tenantA))
}

func TestTenantDeletion_DueTenantsFinalizeHourlyThroughTheService(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	due := testutil.CreateTenant(t, ctx, h.pool, "deletion-due")
	notDue := testutil.CreateTenant(t, ctx, h.pool, "deletion-not-due")
	_, err := h.pool.Exec(ctx,
		`UPDATE tenants SET status = 'pending_deletion', deletion_scheduled_at = NOW() - INTERVAL '1 minute'
		 WHERE id = $1`, due)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`UPDATE tenants SET status = 'pending_deletion', deletion_scheduled_at = NOW() + INTERVAL '1 day'
		 WHERE id = $1`, notDue)
	require.NoError(t, err)

	handler := h.jobs.handler(t, "identity.tenants.finalize_deletions")
	require.NoError(t, handler(ctx, nil))

	var dueStatus, notDueStatus string
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, due).Scan(&dueStatus))
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, notDue).Scan(&notDueStatus))
	assert.Equal(t, "deleted", dueStatus, "a tenant past its grace period is finalized")
	assert.Equal(t, "pending_deletion", notDueStatus, "a tenant inside its grace period stays pending")

	var actorType, reason string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT actor_type, metadata->>'reason' FROM audit_events
		 WHERE tenant_id = $1 AND action = 'tenant.status_changed' ORDER BY id DESC LIMIT 1`,
		due).Scan(&actorType, &reason))
	assert.Equal(t, "system", actorType, "the due transition is a system action")
	assert.Equal(t, "deletion_due", reason)

	var audits int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.status_changed'`, due).Scan(&audits))
	assert.Equal(t, 1, audits)

	// Re-running the hourly job after the transition is a no-op.
	require.NoError(t, handler(ctx, nil))
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.status_changed'`, due).Scan(&audits))
	assert.Equal(t, 1, audits)
}

func TestTrialExpiry_StopsServingAndSuspendsHourly(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	future := time.Now().Add(24 * time.Hour)
	past := time.Now().Add(-time.Minute)
	expired := createTrialTenant(t, ctx, h, "trial-expired", &past)
	live := createTrialTenant(t, ctx, h, "trial-live", &future)
	member := testutil.CreateMember(t, ctx, h.pool, expired, "trial-expired@example.com", testutil.MemberOptions{Role: "owner"})
	token := testutil.SessionToken(t, ctx, h.pool, member, privacyIntegrationSecret)

	// The clock, not the hourly job, decides operational state: an expired
	// trial already returns 404 from tenant resolution and 401 for sessions.
	status, _ := h.request(t, http.MethodGet, "/api/v1/me/profile", token, "trial-expired", "")
	assert.Equal(t, fiber.StatusNotFound, status)

	handler := h.jobs.handler(t, "identity.tenants.expire_trials")
	require.NoError(t, handler(ctx, nil))

	var expiredStatus, liveStatus string
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, expired).Scan(&expiredStatus))
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, live).Scan(&liveStatus))
	assert.Equal(t, "suspended", expiredStatus, "the hourly job suspends an expired trial")
	assert.Equal(t, "trialing", liveStatus, "a live trial is untouched")

	var actorType, reason string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT actor_type, metadata->>'reason' FROM audit_events
		 WHERE tenant_id = $1 AND action = 'tenant.status_changed' ORDER BY id DESC LIMIT 1`,
		expired).Scan(&actorType, &reason))
	assert.Equal(t, "system", actorType, "the transition is audited as a system action")
	assert.Equal(t, "trial_expired", reason)

	// A suspended tenant is not operational: resolution still returns 404, the
	// same response as an unknown tenant (never a hint that it exists).
	status, _ = h.request(t, http.MethodGet, "/api/v1/me/profile", token, "trial-expired", "")
	assert.Equal(t, fiber.StatusNotFound, status)

	// Re-running the job after the status changed is a no-op.
	require.NoError(t, handler(ctx, nil))
}

func TestPrivacy_ExportIncludesArticle15DataWithoutSecrets(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "art15@example.com",
		testutil.MemberOptions{Password: testPassphrase, Role: "admin"})
	inviter := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "art15-inviter@example.com",
		testutil.MemberOptions{Role: "member"})
	token := testutil.SessionToken(t, ctx, h.pool, user, privacyIntegrationSecret)

	var passwordHash string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT password_hash FROM user_passwords WHERE user_id = $1`, user.UserID).Scan(&passwordHash))
	require.NotEmpty(t, passwordHash)

	_, err := h.pool.Exec(ctx,
		`INSERT INTO user_identities (user_id, provider, subject, email) VALUES ($1, 'google', 'google-subject-1', $2)`,
		user.UserID, user.Email)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO mfa_factors (user_id, type, secret_ciphertext, confirmed_at)
		 VALUES ($1, 'totp', 'sealed-totp-secret-must-not-leak', NOW())`, user.UserID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, sha256('recovery-hash-one'::bytea)), ($1, sha256('recovery-hash-two'::bytea))`,
		user.UserID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`UPDATE mfa_recovery_codes SET used_at = NOW() WHERE user_id = $1 AND code_hash = sha256('recovery-hash-one'::bytea)`, user.UserID)
	require.NoError(t, err)

	_, err = h.pool.Exec(ctx,
		`INSERT INTO invitations (tenant_id, email_normalized, role_id, token_hash, invited_by, expires_at)
		 VALUES ($1, 'art15-sent@example.com', (SELECT id FROM roles WHERE tenant_id IS NULL AND scope = 'tenant' AND key = 'member'), sha256('art15-sent-token'::bytea), $2, NOW() + interval '7 days')`,
		tenant.DefaultID, user.UserID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO invitations (tenant_id, email_normalized, role_id, token_hash, invited_by, expires_at, accepted_at, accepted_by)
		 VALUES ($1, $2, (SELECT id FROM roles WHERE tenant_id IS NULL AND scope = 'tenant' AND key = 'member'), sha256('art15-received-token'::bytea), $3, NOW() + interval '7 days', NOW(), $4)`,
		tenant.DefaultID, user.Email, inviter.UserID, user.UserID)
	require.NoError(t, err)

	var roleID uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO roles (tenant_id, scope, key, name) VALUES ($1, 'tenant', 'auditor', 'Auditor') RETURNING id`,
		tenant.DefaultID).Scan(&roleID))
	_, err = h.pool.Exec(ctx,
		`INSERT INTO membership_roles (tenant_id, user_id, role_id) VALUES ($1, $2, $3)`,
		tenant.DefaultID, user.UserID, roleID)
	require.NoError(t, err)

	for i, action := range []string{"files.uploaded", "files.deleted"} {
		_, err = h.pool.Exec(ctx,
			`INSERT INTO audit_events (tenant_id, actor_id, actor_type, action, resource, resource_id, ip, user_agent, created_at)
			 VALUES ($1, $2, 'user', $3, 'file', $4, '203.0.113.7', 'agent-art15', NOW() - ($5 * interval '1 minute'))`,
			tenant.DefaultID, user.UserID, action, strconv.Itoa(i+1), i)
		require.NoError(t, err)
	}

	status, body := h.request(t, http.MethodPost, "/api/v1/me/data-exports", token, "default", "")
	require.Equal(t, fiber.StatusAccepted, status, "body: %s", body)
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.NoError(t, h.runKind(t, "privacy.data_export"))

	status, body = h.request(t, http.MethodGet,
		"/api/v1/me/data-exports/"+created.ID.String()+"/content", token, "default", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	sections := decodeExport(t, body)
	var identity struct {
		Identities []struct {
			Provider string `json:"provider"`
			Subject  string `json:"subject"`
			LinkedAt string `json:"linkedAt"`
		} `json:"identities"`
		MFA struct {
			Enabled                bool   `json:"enabled"`
			ConfirmedAt            string `json:"confirmedAt"`
			RecoveryCodesRemaining int64  `json:"recoveryCodesRemaining"`
		} `json:"mfa"`
		RoleAssignments []struct {
			Key        string `json:"key"`
			AssignedAt string `json:"assignedAt"`
		} `json:"roleAssignments"`
		InvitationsSent []struct {
			Email  string `json:"email"`
			Status string `json:"status"`
		} `json:"invitationsSent"`
		InvitationsReceived []struct {
			Email  string `json:"email"`
			Status string `json:"status"`
		} `json:"invitationsReceived"`
		AuditEvents []struct {
			Action    string `json:"action"`
			CreatedAt string `json:"createdAt"`
			IP        string `json:"ip"`
			UserAgent string `json:"userAgent"`
			Resource  string `json:"resource"`
		} `json:"auditEvents"`
	}
	require.NoError(t, json.Unmarshal(sections["identity"], &identity))
	require.Len(t, identity.Identities, 1)
	assert.Equal(t, "google", identity.Identities[0].Provider)
	assert.Equal(t, "google-subject-1", identity.Identities[0].Subject)
	assert.NotEmpty(t, identity.Identities[0].LinkedAt)
	assert.True(t, identity.MFA.Enabled)
	assert.NotEmpty(t, identity.MFA.ConfirmedAt)
	assert.Equal(t, int64(1), identity.MFA.RecoveryCodesRemaining)
	require.NotEmpty(t, identity.RoleAssignments)
	roleKeys := make([]string, len(identity.RoleAssignments))
	for i := range identity.RoleAssignments {
		roleKeys[i] = identity.RoleAssignments[i].Key
		assert.NotEmpty(t, identity.RoleAssignments[i].AssignedAt)
	}
	assert.Contains(t, roleKeys, "auditor", "the user's current role assignments are exported")
	assert.Contains(t, roleKeys, "admin")
	require.Len(t, identity.InvitationsSent, 1)
	assert.Equal(t, "art15-sent@example.com", identity.InvitationsSent[0].Email)
	assert.Equal(t, "pending", identity.InvitationsSent[0].Status)
	require.Len(t, identity.InvitationsReceived, 1)
	assert.Equal(t, user.Email, identity.InvitationsReceived[0].Email)
	assert.Equal(t, "accepted", identity.InvitationsReceived[0].Status)
	require.NotEmpty(t, identity.AuditEvents)
	byAction := map[string]struct {
		IP, UserAgent, Resource, CreatedAt string
	}{}
	var previous time.Time
	for _, event := range identity.AuditEvents {
		parsed, err := time.Parse(time.RFC3339Nano, event.CreatedAt)
		require.NoError(t, err)
		if !previous.IsZero() {
			assert.False(t, parsed.After(previous), "audit events are newest first")
		}
		previous = parsed
		byAction[event.Action] = struct {
			IP, UserAgent, Resource, CreatedAt string
		}{event.IP, event.UserAgent, event.Resource, event.CreatedAt}
	}
	uploaded, ok := byAction["files.uploaded"]
	require.True(t, ok, "the user's own audit row is exported")
	assert.Equal(t, "203.0.113.7", uploaded.IP)
	assert.Equal(t, "agent-art15", uploaded.UserAgent)
	assert.Equal(t, "file", uploaded.Resource)

	leaked := string(body)
	for _, secret := range []string{
		"sealed-totp-secret-must-not-leak", "recovery-hash-one", "recovery-hash-two", passwordHash,
	} {
		assert.NotContains(t, leaked, secret, "the export must never contain credentials or secrets")
	}
}
