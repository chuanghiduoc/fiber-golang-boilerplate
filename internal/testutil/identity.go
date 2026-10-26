//go:build integration

package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// Member is a fixture user with a membership in one tenant (users
// are global identities; tenant-owned rows reference memberships).
type Member struct {
	UserID       int64
	TenantID     int64
	Email        string
	Role         string
	TokenVersion int32
}

// MemberOptions tune CreateMember. The zero value creates a verified,
// passwordless, active "member".
type MemberOptions struct {
	Role string // system role key: "member" (default), "admin", or "owner"
	// Password, when set, stores a bcrypt (MinCost) hash in user_passwords.
	Password   string
	Unverified bool
}

// CreateMember inserts a global user (or reuses the live user with the same
// email) and an active membership in tenantID. Emails are normalized to
// lower case like the identity service does.
func CreateMember(t testing.TB, ctx context.Context, pool *pgxpool.Pool, tenantID int64, email string, opts MemberOptions) Member {
	t.Helper()
	m, err := createMember(ctx, pool, tenantID, email, opts)
	if err != nil {
		t.Fatalf("create member %s in tenant %d: %v", email, tenantID, err)
	}
	return m
}

func createMember(ctx context.Context, pool *pgxpool.Pool, tenantID int64, email string, opts MemberOptions) (Member, error) {
	role := opts.Role
	if role == "" || role == "user" {
		role = "member"
	}
	email = strings.ToLower(strings.TrimSpace(email))
	m := Member{TenantID: tenantID, Email: email, Role: role}
	err := pool.QueryRow(ctx, `
		WITH existing AS (
			SELECT id, token_version FROM users WHERE email = $1 AND status <> 'deleted'
		), created AS (
			INSERT INTO users (email, name, email_verified_at)
			SELECT $1, $1, CASE WHEN $2::boolean THEN NULL ELSE NOW() END
			WHERE NOT EXISTS (SELECT 1 FROM existing)
			RETURNING id, token_version
		)
		SELECT id, token_version FROM existing UNION ALL SELECT id, token_version FROM created`,
		email, opts.Unverified).Scan(&m.UserID, &m.TokenVersion)
	if err != nil {
		return Member{}, fmt.Errorf("upsert user: %w", err)
	}
	if opts.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(opts.Password), bcrypt.MinCost)
		if err != nil {
			return Member{}, err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_passwords (user_id, password_hash) VALUES ($1, $2)
			ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash`,
			m.UserID, string(hash)); err != nil {
			return Member{}, fmt.Errorf("set password: %w", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`,
		tenantID, m.UserID); err != nil {
		return Member{}, fmt.Errorf("create membership: %w", err)
	}
	tag, err := pool.Exec(ctx, `
		INSERT INTO membership_roles (tenant_id, user_id, role_id)
		SELECT $1, $2, r.id FROM roles r
		WHERE r.tenant_id IS NULL AND r.scope = 'tenant' AND r.key = $3`,
		tenantID, m.UserID, role)
	if err != nil {
		return Member{}, fmt.Errorf("assign role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Member{}, fmt.Errorf("unknown role %q", role)
	}
	return m, nil
}

// CreateTenant inserts an active tenant (and its settings row) and returns
// its id.
func CreateTenant(t testing.TB, ctx context.Context, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("create tenant settings %s: %v", slug, err)
	}
	return id
}

// SessionToken opens a password session for m (reading the user's current
// token version) and mints an access token for it, the way a real sign-in
// does. The session validator rejects access tokens without a live session.
func SessionToken(t testing.TB, ctx context.Context, pool *pgxpool.Pool, m Member, secret string) string {
	t.Helper()
	return SessionTokenAMR(t, ctx, pool, m, secret, []string{"pwd"})
}

// SessionTokenAMR is SessionToken with the given authentication methods on
// both the session and the token, e.g. {"pwd", "otp"} after a second factor.
func SessionTokenAMR(t testing.TB, ctx context.Context, pool *pgxpool.Pool, m Member, secret string, amr []string) string {
	t.Helper()
	var (
		sessionID int64
		version   int32
	)
	err := pool.QueryRow(ctx, `
		INSERT INTO sessions (tenant_id, user_id, token_version, expires_at, amr)
		SELECT m.tenant_id, m.user_id, u.token_version, NOW() + INTERVAL '720 hours', $3
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.user_id = $2
		RETURNING id, token_version`,
		m.TenantID, m.UserID, amr).Scan(&sessionID, &version)
	if err != nil {
		t.Fatalf("create session for user %d in tenant %d: %v", m.UserID, m.TenantID, err)
	}
	tok, err := token.GenerateForSession(token.SessionClaims{
		UserID:       m.UserID,
		TenantID:     m.TenantID,
		SessionID:    sessionID,
		Email:        m.Email,
		TokenVersion: version,
		AMR:          amr,
	}, secret, 1)
	if err != nil {
		t.Fatalf("sign session token: %v", err)
	}
	return tok
}
