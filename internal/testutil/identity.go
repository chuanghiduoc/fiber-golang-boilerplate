//go:build integration

package testutil

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// Member is a fixture user with a membership in one tenant (users
// are global identities; tenant-owned rows reference memberships).
type Member struct {
	UserID       uuid.UUID
	TenantID     uuid.UUID
	Email        string
	Role         string
	TokenVersion int32
}

// MemberOptions tune CreateMember. The zero value creates a verified,
// passwordless, active "member".
type MemberOptions struct {
	Role string // system role key: "member" (default), "admin", or "owner"
	// Password, when set, stores a cheap-parameter Argon2id hash in
	// user_passwords.
	Password   string
	Unverified bool
}

// CreateMember inserts a global user (or reuses the live user with the same
// email) and an active membership in tenantID. Emails are normalized to
// lower case like the identity service does.
func CreateMember(t testing.TB, ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, email string, opts MemberOptions) Member {
	t.Helper()
	m, err := createMember(ctx, pool, tenantID, email, opts)
	if err != nil {
		t.Fatalf("create member %s in tenant %s: %v", email, tenantID, err)
	}
	return m
}

func createMember(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, email string, opts MemberOptions) (Member, error) {
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
		hash, err := testPasswordHash(opts.Password)
		if err != nil {
			return Member{}, err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO user_passwords (user_id, password_hash) VALUES ($1, $2)
			ON CONFLICT (user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash`,
			m.UserID, hash); err != nil {
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

// testPasswordHash builds a real Argon2id PHC string with the cheap fixture
// parameters. Integration sign-ins verify it and rehash it at the production
// parameters on success, exactly like a legacy hash.
func testPasswordHash(password string) (string, error) {
	const (
		memory      = 1024
		iterations  = 1
		parallelism = 1
		saltLength  = 16
		keyLength   = 32
	)
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(validator.NormalizePassword(password)), salt, iterations, memory, parallelism, keyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CreateTenant inserts an active tenant (and its settings row) and returns
// its id.
func CreateTenant(t testing.TB, ctx context.Context, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
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
		sessionID uuid.UUID
		version   int32
	)
	err := pool.QueryRow(ctx, `
		INSERT INTO sessions (tenant_id, user_id, token_version, expires_at, amr, mfa_verified_at)
		SELECT m.tenant_id, m.user_id, u.token_version, NOW() + INTERVAL '720 hours', $3,
		       CASE WHEN $3::text[] && ARRAY['otp', 'rc', 'hwk', 'swk']::text[] THEN NOW() ELSE NULL END
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.user_id = $2
		RETURNING id, token_version`,
		m.TenantID, m.UserID, amr).Scan(&sessionID, &version)
	if err != nil {
		t.Fatalf("create session for user %s in tenant %s: %v", m.UserID, m.TenantID, err)
	}
	tok, err := token.GenerateForSession(token.SessionClaims{
		UserID:       m.UserID,
		TenantID:     m.TenantID,
		SessionID:    sessionID,
		TokenVersion: version,
		AMR:          amr,
	}, secret, time.Hour)
	if err != nil {
		t.Fatalf("sign session token: %v", err)
	}
	return tok
}
