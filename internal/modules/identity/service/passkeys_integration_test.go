//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/softauthenticator"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/google/uuid"
)

const (
	integrationPasskeyRPID   = "localhost"
	integrationPasskeyOrigin = "http://localhost:3000"
)

func registerPasskeyUser(t *testing.T, e *env, email string) *service.Member {
	t.Helper()
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member, err := e.users.Register(ctx, service.RegisterInput{
		Email: email, Password: testPassphrase, Name: "Passkey Tester",
	})
	require.NoError(t, err)
	return member
}

func TestPasskeyRegistrationLoginAndCounterPersistence(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-integration@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)

	creation, err := e.passkeys.BeginRegistration(ctx, member.User.ID)
	require.NoError(t, err)
	passkey, err := e.passkeys.FinishRegistration(ctx, member.User.ID, "Laptop",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, passkey.ID)

	var storedUserID uuid.UUID
	var storedID, storedKey []byte
	var transports []string
	var backupEligible bool
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT user_id, credential_id, public_key, transports, backup_eligible
		 FROM webauthn_credentials WHERE id = $1`, passkey.ID).
		Scan(&storedUserID, &storedID, &storedKey, &transports, &backupEligible))
	assert.Equal(t, member.User.ID, storedUserID)
	assert.Equal(t, authenticator.CredentialID, storedID)
	assert.NotEmpty(t, storedKey)
	assert.Equal(t, []string{"internal"}, transports)
	assert.False(t, backupEligible)

	assertion, err := e.passkeys.BeginLogin(ctx, "")
	require.NoError(t, err)
	login, err := e.passkeys.FinishLogin(ctx,
		authenticator.Assertion(t, assertion.Response.Challenge.String(), member.User.ID[:]), nil)
	require.NoError(t, err)
	assert.Equal(t, service.PasskeyMethodHardware, login.Method)

	var amr []string
	var verifiedAt *time.Time
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT amr, mfa_verified_at FROM sessions WHERE id = $1`, login.Tokens.SessionID).
		Scan(&amr, &verifiedAt))
	assert.Contains(t, amr, service.PasskeyMethodHardware)
	assert.NotNil(t, verifiedAt)

	var counter int64
	var lastUsed *time.Time
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT sign_count, last_used_at FROM webauthn_credentials WHERE id = $1`, passkey.ID).
		Scan(&counter, &lastUsed))
	assert.Equal(t, int64(1), counter)
	assert.NotNil(t, lastUsed)
}

func TestPasskeySyncedCredentialRecordsSoftwareAMR(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "synced-passkey@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)
	authenticator.BackupEligible = true
	authenticator.BackupState = true

	creation, err := e.passkeys.BeginRegistration(ctx, member.User.ID)
	require.NoError(t, err)
	_, err = e.passkeys.FinishRegistration(ctx, member.User.ID, "Phone",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)

	assertion, err := e.passkeys.BeginLogin(ctx, "")
	require.NoError(t, err)
	login, err := e.passkeys.FinishLogin(ctx,
		authenticator.Assertion(t, assertion.Response.Challenge.String(), member.User.ID[:]), nil)
	require.NoError(t, err)
	assert.Equal(t, service.PasskeyMethodSoftware, login.Method)
}

func TestPasskeyExportListsMetadataOnlyAndErasureDeletes(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-erasure@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)
	creation, err := e.passkeys.BeginRegistration(ctx, member.User.ID)
	require.NoError(t, err)
	_, err = e.passkeys.FinishRegistration(ctx, member.User.ID, "YubiKey",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)

	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	privacy := service.NewPrivacyService(service.PrivacyDeps{
		Stores: e.stores, Tx: e.tx, Storage: store,
		ExportStore: identityrepo.NewPrivacyExportRepository(e.fx.Scoped()),
	})
	section, err := privacy.Export(ctx, tenant.DefaultID, member.User.ID)
	require.NoError(t, err)
	var exported struct {
		Passkeys []struct {
			Name       string     `json:"name"`
			CreatedAt  time.Time  `json:"createdAt"`
			LastUsedAt *time.Time `json:"lastUsedAt"`
		} `json:"passkeys"`
	}
	require.NoError(t, json.Unmarshal(section.Data, &exported))
	require.Len(t, exported.Passkeys, 1)
	assert.Equal(t, "YubiKey", exported.Passkeys[0].Name)
	assert.False(t, exported.Passkeys[0].CreatedAt.IsZero())
	lowered := strings.ToLower(string(section.Data))
	assert.NotContains(t, lowered, "publickey")
	assert.NotContains(t, lowered, "credentialid")
	assert.NotContains(t, lowered, "aaguid")

	// The global finalizer deletes passkeys when the last membership has been
	// removed (the user row itself is only anonymized, so no cascade fires).
	_, err = e.stores.Memberships.Remove(ctx, member.User.ID, service.BanMarker{})
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]string{"userId": member.User.ID.String()})
	require.NoError(t, err)
	require.NoError(t, privacy.HandleFinalizeJob(ctx, payload))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM webauthn_credentials WHERE user_id = $1`, member.User.ID))
}
