//go:build integration

package authn_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/google/uuid"
)

const probeHandler = "probe"

func subscribeProbe(t *testing.T, bus *events.Bus) {
	t.Helper()
	require.NoError(t, bus.SubscribeDurable(identitycontracts.EventUserRegistered, probeHandler,
		func(context.Context, events.Event) error { return nil }))
}

// Registration publishes user.registered through the identity transactor:
// the durable subscriber job commits with the account, carries the envelope,
// and is scoped to the tenant of the new membership.
func TestRegister_PublishesUserRegisteredInTransaction(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "acme")

	bus := events.New(jobs.NewQueue(e.Fx.Scoped()))
	subscribeProbe(t, bus)
	tx := identityrepo.NewTransactor(e.Fx.Tx(), audit.NewPostgresRecorder(e.Fx.Scoped()), nil, bus, testutil.BanHasher(t))
	users := authn.NewUserService(authn.UserDeps{Stores: e.Stores, Tx: tx, Cache: cache.NewMemoryCache()})

	user, err := users.Register(tenant.WithID(ctx, acmeID), authn.RegisterInput{Email: "event@example.com", Password: testPassphrase, Name: "Event"})
	require.NoError(t, err)

	var (
		jobTenant uuid.UUID
		payload   []byte
	)
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT tenant_id, payload FROM jobs WHERE kind = $1`, "events.user.registered."+probeHandler,
	).Scan(&jobTenant, &payload))
	assert.Equal(t, acmeID, jobTenant)

	var envelope events.Event
	require.NoError(t, json.Unmarshal(payload, &envelope))
	assert.Equal(t, identitycontracts.EventUserRegistered, envelope.Type)
	assert.Equal(t, identitycontracts.UserRegisteredVersion, envelope.DataVersion)
	assert.Equal(t, acmeID, envelope.TenantID)
	assert.Equal(t, events.UserActor(user.User.ID), envelope.Actor)
	data, err := events.Decode[identitycontracts.UserRegistered](envelope)
	require.NoError(t, err)
	assert.Equal(t, user.User.ID, data.UserID)
	assert.Equal(t, "event@example.com", data.Email)
	assert.Equal(t, user.User.ID.String(), envelope.Actor.ID)
}

// A publish failure (here a durable subscriber with no queue) fails the
// registration closed: user, password, membership, and audit row roll back.
func TestRegister_RollsBackWhenPublishFails(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := context.Background()

	bus := events.New(nil) // durable publish fails with events.ErrNoQueue
	subscribeProbe(t, bus)
	tx := identityrepo.NewTransactor(e.Fx.Tx(), audit.NewPostgresRecorder(e.Fx.Scoped()), nil, bus, testutil.BanHasher(t))
	users := authn.NewUserService(authn.UserDeps{Stores: e.Stores, Tx: tx, Cache: cache.NewMemoryCache()})

	_, err := users.Register(ctx, authn.RegisterInput{Email: "rollback@example.com", Password: testPassphrase, Name: "Rollback"})
	require.Error(t, err)
	assert.Equal(t, 500, pgfixtures.AppStatus(err))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM users`))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM memberships`))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM user_passwords`))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM audit_events WHERE action = 'auth.registered'`))
}
