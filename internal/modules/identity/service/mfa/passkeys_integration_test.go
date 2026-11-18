//go:build integration

package mfa_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/softauthenticator"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/google/uuid"
)

const (
	integrationPasskeyRPID   = "localhost"
	integrationPasskeyOrigin = "http://localhost:3000"
)

func registerPasskeyUser(t *testing.T, e *pgfixtures.Env, email string) *core.Member {
	t.Helper()
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member, err := e.Users.Register(ctx, authn.RegisterInput{
		Email: email, Password: testPassphrase, Name: "Passkey Tester",
	})
	require.NoError(t, err)
	return member
}

func passkeyReauthProof(t *testing.T, e *pgfixtures.Env, ctx context.Context, user core.User) core.ReauthProof {
	t.Helper()
	pair, err := e.Sessions.Issue(ctx, user.ID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)
	return core.ReauthProof{SessionID: pair.SessionID, CurrentPassword: testPassphrase}
}

func TestPasskeyRegistrationLoginAndCounterPersistence(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-integration@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)

	creation, err := e.Passkeys.BeginRegistration(ctx, member.User.ID, passkeyReauthProof(t, e, ctx, member.User))
	require.NoError(t, err)
	passkey, err := e.Passkeys.FinishRegistration(ctx, member.User.ID, "Laptop",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, passkey.ID)

	var storedUserID uuid.UUID
	var storedID, storedKey []byte
	var transports []string
	var backupEligible bool
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT user_id, credential_id, public_key, transports, backup_eligible
		 FROM webauthn_credentials WHERE id = $1`, passkey.ID).
		Scan(&storedUserID, &storedID, &storedKey, &transports, &backupEligible))
	assert.Equal(t, member.User.ID, storedUserID)
	assert.Equal(t, authenticator.CredentialID, storedID)
	assert.NotEmpty(t, storedKey)
	assert.Equal(t, []string{"internal"}, transports)
	assert.False(t, backupEligible)

	assertion, err := e.Passkeys.BeginLogin(ctx, "")
	require.NoError(t, err)
	login, err := e.Passkeys.FinishLogin(ctx,
		authenticator.Assertion(t, assertion.Response.Challenge.String(), member.User.ID[:]), nil)
	require.NoError(t, err)
	assert.Equal(t, core.PasskeyMethodHardware, login.Method)

	var amr []string
	var verifiedAt *time.Time
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT amr, mfa_verified_at FROM sessions WHERE id = $1`, login.Tokens.SessionID).
		Scan(&amr, &verifiedAt))
	assert.Contains(t, amr, core.PasskeyMethodHardware)
	assert.NotNil(t, verifiedAt)

	var counter int64
	var lastUsed *time.Time
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT sign_count, last_used_at FROM webauthn_credentials WHERE id = $1`, passkey.ID).
		Scan(&counter, &lastUsed))
	assert.Equal(t, int64(1), counter)
	assert.NotNil(t, lastUsed)
}

func TestPasskeySyncedCredentialRecordsSoftwareAMR(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "synced-passkey@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)
	authenticator.BackupEligible = true
	authenticator.BackupState = true

	creation, err := e.Passkeys.BeginRegistration(ctx, member.User.ID, passkeyReauthProof(t, e, ctx, member.User))
	require.NoError(t, err)
	_, err = e.Passkeys.FinishRegistration(ctx, member.User.ID, "Phone",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)

	assertion, err := e.Passkeys.BeginLogin(ctx, "")
	require.NoError(t, err)
	login, err := e.Passkeys.FinishLogin(ctx,
		authenticator.Assertion(t, assertion.Response.Challenge.String(), member.User.ID[:]), nil)
	require.NoError(t, err)
	assert.Equal(t, core.PasskeyMethodSoftware, login.Method)
}

func TestPasskeyExportListsMetadataOnlyAndErasureDeletes(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-erasure@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)
	creation, err := e.Passkeys.BeginRegistration(ctx, member.User.ID, passkeyReauthProof(t, e, ctx, member.User))
	require.NoError(t, err)
	_, err = e.Passkeys.FinishRegistration(ctx, member.User.ID, "YubiKey",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)

	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	privacy := privacy.NewPrivacyService(privacy.PrivacyDeps{
		Stores: e.Stores, Tx: e.Tx, Storage: store,
		ExportStore: identityrepo.NewPrivacyExportRepository(e.Fx.Scoped()),
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
	_, err = e.Stores.Memberships.Remove(ctx, member.User.ID, core.BanMarker{})
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]string{"userId": member.User.ID.String()})
	require.NoError(t, err)
	require.NoError(t, privacy.HandleFinalizeJob(ctx, payload))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM webauthn_credentials WHERE user_id = $1`, member.User.ID))
}

// The step-up statement keeps the session's other methods but only the
// passkey method it proved. A TOTP or recovery-code step-up therefore removes
// hwk/swk, so a passkey method on a session always means that
// mfa_verified_at came from a passkey.
func TestStepUpKeepsOnlyTheProvenPasskeyMethod(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-stepup-amr@example.com")
	pair, err := e.Sessions.Issue(ctx, member.User.ID, member.User.TokenVersion,
		[]string{core.AMRPassword, core.PasskeyMethodHardware, core.PasskeyMethodSoftware})
	require.NoError(t, err)
	sessionAMR := func() []string {
		var amr []string
		require.NoError(t, e.Pool.QueryRow(context.Background(),
			`SELECT amr FROM sessions WHERE id = $1`, pair.SessionID).Scan(&amr))
		return amr
	}

	_, err = e.Stores.Sessions.MarkMFAVerified(ctx, member.User.ID, pair.SessionID, core.AMROTP)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{core.AMRPassword, core.AMROTP}, sessionAMR(),
		"a code step-up removes every passkey method")

	_, err = e.Stores.Sessions.MarkMFAVerified(ctx, member.User.ID, pair.SessionID, core.PasskeyMethodHardware)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{core.AMRPassword, core.AMROTP, core.PasskeyMethodHardware}, sessionAMR(),
		"a passkey step-up adds the method it proved")

	_, err = e.Stores.Sessions.MarkMFAVerified(ctx, member.User.ID, pair.SessionID, core.PasskeyMethodSoftware)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{core.AMRPassword, core.AMROTP, core.PasskeyMethodSoftware}, sessionAMR(),
		"only the passkey method of the latest step-up stays")
}

// Once a passkey exists, a session that proved only the password cannot add
// another one, and the platform MFA reset (the recovery path for a lost
// passkey) removes every passkey.
func TestPasskeyChangeNeedsAPasskeySessionAndResetDeletesPasskeys(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-assurance@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)
	creation, err := e.Passkeys.BeginRegistration(ctx, member.User.ID, passkeyReauthProof(t, e, ctx, member.User))
	require.NoError(t, err, "the first passkey needs only the re-authenticated session")
	_, err = e.Passkeys.FinishRegistration(ctx, member.User.ID, "First",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	require.NoError(t, err)

	_, err = e.Passkeys.BeginRegistration(ctx, member.User.ID, passkeyReauthProof(t, e, ctx, member.User))
	assert.Equal(t, string(apperror.CodeMFARequired), pgfixtures.AppCode(err),
		"a first-factor session cannot add a passkey to an account that has one")

	proven, err := e.Sessions.Issue(ctx, member.User.ID, member.User.TokenVersion,
		[]string{core.AMRPassword, core.PasskeyMethodHardware})
	require.NoError(t, err)
	_, err = e.Passkeys.BeginRegistration(ctx, member.User.ID,
		core.ReauthProof{SessionID: proven.SessionID, CurrentPassword: testPassphrase})
	require.NoError(t, err, "a fresh passkey session can add one")

	require.NoError(t, e.MFA.ResetTOTP(ctx, member.User.ID))
	var remaining int64
	require.NoError(t, e.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webauthn_credentials WHERE user_id = $1`, member.User.ID).Scan(&remaining))
	assert.Zero(t, remaining, "the platform reset removes every passkey")
}

// A finalizer that runs while a membership remains fails and retries. The
// account-scoped erase job also queues a new finalizer after it removes its
// membership, so the account is anonymized when the retries have ended.
func TestSelfErasureEraseJobQueuesTheFinalizerInDatabase(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "erasure-finalizer@example.com")
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	tx := identityrepo.NewTransactorWithQueue(e.Fx.Tx(), audit.NewPostgresRecorder(e.Fx.Scoped()), e.Mail, nil,
		jobs.NewQueue(e.Fx.Scoped()), testutil.BanHasher(t))
	registry := privacycontracts.NewRegistry()
	svc := privacy.NewPrivacyService(privacy.PrivacyDeps{
		Stores: e.Stores, Tx: tx, Storage: store, Registry: registry,
		ExportStore: identityrepo.NewPrivacyExportRepository(e.Fx.Scoped()),
	})
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: svc, Eraser: svc})
	queued := func(kind string) int64 {
		return scalar[int64](t, e.Pool, `SELECT count(*) FROM jobs WHERE kind = $1`, kind)
	}
	finalizePayload, err := json.Marshal(privacy.FinalizeJob{UserID: member.User.ID})
	require.NoError(t, err)

	require.NoError(t, svc.RequestErasure(ctx, member.User.ID, "user.erasure_requested"))
	require.Equal(t, int64(1), queued(privacy.JobKindErase))
	require.Equal(t, int64(1), queued(privacy.JobKindEraseFinalize))

	require.ErrorIs(t, svc.HandleFinalizeJob(ctx, finalizePayload), privacy.ErrErasurePending,
		"a finalizer that runs before the erase job must retry")
	assert.Equal(t, "erasure-finalizer@example.com",
		scalar[string](t, e.Pool, `SELECT email FROM users WHERE id = $1`, member.User.ID),
		"the account is untouched while a membership remains")

	erasePayload := scalar[[]byte](t, e.Pool, `SELECT payload FROM jobs WHERE kind = $1`, privacy.JobKindErase)
	require.NoError(t, svc.HandleEraseJob(ctx, erasePayload))
	assert.Equal(t, int64(2), queued(privacy.JobKindEraseFinalize),
		"the erase job queues a finalizer after it removes the membership")

	require.NoError(t, svc.HandleFinalizeJob(ctx, finalizePayload))
	assert.NotEqual(t, "erasure-finalizer@example.com",
		scalar[string](t, e.Pool, `SELECT email FROM users WHERE id = $1`, member.User.ID),
		"the finalizer anonymizes the account once every membership is removed")
}

// The re-issue after an MFA change merges the new method into the amr the
// session row has at write time. It must not write back a value read earlier:
// that could restore a passkey method a concurrent code step-up removed.
func TestRestampMergesTheMethodIntoTheCurrentAMR(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-restamp@example.com")
	pair, err := e.Sessions.Issue(ctx, member.User.ID, member.User.TokenVersion,
		[]string{core.AMRPassword, core.PasskeyMethodHardware})
	require.NoError(t, err)
	// A code step-up commits after the caller read the session.
	_, err = e.Stores.Sessions.MarkMFAVerified(ctx, member.User.ID, pair.SessionID, core.AMRRecoveryCode)
	require.NoError(t, err)

	restamped, err := e.Stores.Sessions.Restamp(ctx, member.User.ID, pair.SessionID, member.User.TokenVersion, core.AMROTP)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{core.AMRPassword, core.AMRRecoveryCode, core.AMROTP}, restamped.AMR,
		"the restamp adds its method and does not restore the removed passkey method")
}

// The factor check and the insert of a registration run under the per-user
// passkey lock. Without it, two registrations that start while the account has
// no passkey both read "no passkey" and both pass the first-passkey rule. The
// test holds the lock on another connection and checks that the registration
// waits for it.
func TestPasskeyRegistrationWaitsForThePerUserLock(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := registerPasskeyUser(t, e, "passkey-lock@example.com")
	authenticator := softauthenticator.New(t, integrationPasskeyRPID, integrationPasskeyOrigin)
	creation, err := e.Passkeys.BeginRegistration(ctx, member.User.ID, passkeyReauthProof(t, e, ctx, member.User))
	require.NoError(t, err)
	attestation := authenticator.Attestation(t, creation.Response.Challenge.String())

	holder, err := e.Pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.Background()) }()
	_, err = holder.Exec(context.Background(),
		`SELECT pg_advisory_xact_lock(hashtextextended('identity.passkey_changes:' || $1::uuid, 0))`, member.User.ID)
	require.NoError(t, err)

	finished := make(chan error, 1)
	go func() {
		_, err := e.Passkeys.FinishRegistration(ctx, member.User.ID, "Waiting key", attestation)
		finished <- err
	}()
	select {
	case err := <-finished:
		t.Fatalf("registration finished while the passkey lock was held (err = %v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	require.NoError(t, holder.Rollback(context.Background()))
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("registration did not finish after the lock was released")
	}
}
