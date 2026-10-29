package seed_test

import (
	"context"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
)

func TestAdminRejectsWeakPasswordBeforeTouchingStores(t *testing.T) {
	err := seed.Admin(context.Background(), config.AdminConfig{
		Email:    "admin@example.com",
		Password: "short",
	}, core.Stores{}, nil)
	if err == nil {
		t.Fatal("Admin accepted a password below the 12-rune minimum")
	}
	if !strings.Contains(err.Error(), "password policy") {
		t.Fatalf("error = %q, want a password policy failure", err)
	}
}
