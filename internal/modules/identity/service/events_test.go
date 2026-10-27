package service

import (
	"context"
	"errors"
	"testing"
	"time"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/google/uuid"
)

// The user.registered envelope carries the contracts payload (not the REST
// DTO), the new user as actor, and the membership's tenant. Transactional
// publishing is covered by the integration suite.
func TestUserRegisteredEvent(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	event, err := userRegisteredEvent(&Member{
		User: User{
			ID: testID(7), Email: "new@example.com", Name: "New User",
			Status: UserStatusActive, TokenVersion: 4, HasPassword: true,
			CreatedAt: created,
		},
		Membership: Membership{ID: testID(9), TenantID: testID(3), UserID: testID(7), Role: RoleMember, Status: MembershipStatusActive},
	})
	if err != nil {
		t.Fatalf("userRegisteredEvent: %v", err)
	}
	if event.Type != identitycontracts.EventUserRegistered || event.DataVersion != identitycontracts.UserRegisteredVersion {
		t.Errorf("event = %s v%d", event.Type, event.DataVersion)
	}
	if event.TenantID != testID(3) || event.Actor.Type != "user" || event.Actor.ID != testID(7).String() || event.ID == uuid.Nil {
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
	db := newMemDB()
	ctx := tenant.WithID(context.Background(), testID(2))
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "e@example.com", Password: testPassphrase, Name: "E"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(db.published) != 1 {
		t.Fatalf("published = %d events, want 1", len(db.published))
	}
	event := db.published[0]
	if event.Type != identitycontracts.EventUserRegistered || event.TenantID != testID(2) || event.Actor.ID != user.User.ID.String() {
		t.Errorf("event = %+v", event)
	}
}

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, events.Event) error {
	return errors.New("queue down")
}

// A publish failure is returned, so the registration transaction rolls back.
func TestPublishRegisteredTx_FailsClosed(t *testing.T) {
	err := publishRegisteredTx(testTenantCtx(), failingPublisher{}, &Member{
		User:       User{ID: testID(1), Email: "a@example.com", CreatedAt: time.Now()},
		Membership: Membership{TenantID: testID(1)},
	})
	if err == nil {
		t.Fatal("publishRegisteredTx: want error when the publisher fails")
	}
}
