package service

import (
	"strings"
	"testing"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
)

func TestAuthenticateRehashesAnOlderHash(t *testing.T) {
	db := newMemDB()
	svc := newTestUserService(db)
	ctx := testTenantCtx()
	member, err := svc.Register(ctx, RegisterInput{
		Email: "rehash@example.com", Password: testPassphrase, Name: "Rehash",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	db.passwords[member.User.ID] = core.EncodeArgon2idWithParams(testPassphrase, 2048, 3, 1)

	if _, err := svc.Authenticate(ctx, LoginInput{Email: "rehash@example.com", Password: testPassphrase}); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	upgraded := db.passwords[member.User.ID]
	if !strings.HasPrefix(upgraded, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("stored hash = %q, want an upgrade to the current parameters", upgraded)
	}
}
