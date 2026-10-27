package authn

import (
	"strings"
	"testing"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
)

func TestAuthenticateRehashesAnOlderHash(t *testing.T) {
	db := fakes.New()
	svc := newTestUserService(db)
	ctx := fakes.TenantCtx()
	member, err := svc.Register(ctx, RegisterInput{
		Email: "rehash@example.com", Password: fakes.Passphrase, Name: "Rehash",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	db.Passwords[member.User.ID] = core.EncodeArgon2idWithParams(fakes.Passphrase, 2048, 3, 1)

	if _, err := svc.Authenticate(ctx, LoginInput{Email: "rehash@example.com", Password: fakes.Passphrase}); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	upgraded := db.Passwords[member.User.ID]
	if !strings.HasPrefix(upgraded, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("stored hash = %q, want an upgrade to the current parameters", upgraded)
	}
}
