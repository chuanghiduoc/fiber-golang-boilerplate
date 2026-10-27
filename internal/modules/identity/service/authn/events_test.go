package authn

import (
	"context"
	"errors"
	"testing"
	"time"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/google/uuid"
)

// The user.registered envelope carries the contracts payload (not the REST
// DTO), the new user as actor, and the membership's tenant. Transactional
// publishing is covered by the integration suite.
func TestUserRegisteredEvent(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	event, err := UserRegisteredEvent(&core.Member{
		User: core.User{
			ID: fakes.TestID(7), Email: "new@example.com", Name: "New User",
			Status: core.UserStatusActive, TokenVersion: 4, HasPassword: true,
			CreatedAt: created,
		},
		Membership: core.Membership{ID: fakes.TestID(9), TenantID: fakes.TestID(3), UserID: fakes.TestID(7), Role: core.RoleMember, Status: core.MembershipStatusActive},
	})
	if err != nil {
		t.Fatalf("userRegisteredEvent: %v", err)
	}
	if event.Type != identitycontracts.EventUserRegistered || event.DataVersion != identitycontracts.UserRegisteredVersion {
		t.Errorf("event = %s v%d", event.Type, event.DataVersion)
	}
	if event.TenantID != fakes.TestID(3) || event.Actor.Type != "user" || event.Actor.ID != fakes.TestID(7).String() || event.ID == uuid.Nil {
		t.Errorf("envelope = %+v", event)
	}
	const want = `{"userId":"0192c9b0-7e4a-7000-8000-000000000007","email":"new@example.com","name":"New User","emailVerified":false,"createdAt":"2026-01-02T03:04:05Z"}`
	if string(event.Data) != want {
		t.Errorf("data =\n%s\nwant\n%s", event.Data, want)
	}
}

// Register publishes exactly one user.registered event through the
// registration transaction, scoped to the request tenant.
func TestRegister_PublishesUserRegisteredInRequestTenant(t *testing.T) {
	db := fakes.New()
	ctx := tenant.WithID(context.Background(), fakes.TestID(2))
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "e@example.com", Password: fakes.Passphrase, Name: "E"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(db.Published) != 1 {
		t.Fatalf("published = %d events, want 1", len(db.Published))
	}
	event := db.Published[0]
	if event.Type != identitycontracts.EventUserRegistered || event.TenantID != fakes.TestID(2) || event.Actor.ID != user.User.ID.String() {
		t.Errorf("event = %+v", event)
	}
}

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, events.Event) error {
	return errors.New("queue down")
}

// A publish failure is returned, so the registration transaction rolls back.
func TestPublishRegisteredTx_FailsClosed(t *testing.T) {
	err := PublishRegisteredTx(fakes.TenantCtx(), failingPublisher{}, &core.Member{
		User:       core.User{ID: fakes.TestID(1), Email: "a@example.com", CreatedAt: time.Now()},
		Membership: core.Membership{TenantID: fakes.TestID(1)},
	})
	if err == nil {
		t.Fatal("publishRegisteredTx: want error when the publisher fails")
	}
}
