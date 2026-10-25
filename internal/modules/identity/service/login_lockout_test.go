package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return appErr.Status
	}
	return -1
}

func newTestUserService(db *memDB) UserService {
	return NewUserService(UserDeps{Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache()})
}

// Parallel wrong-password attempts must not all pass a stale "not locked yet"
// read: the counter is reserved atomically before the bcrypt comparison.
func TestAuthenticate_ConcurrentAttemptsCannotExceedLockout(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := context.Background()
	if _, err := svc.Register(ctx, dto.RegisterRequest{
		Email: "race@example.com", Password: "Password1!", Name: "Race",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	const attempts = maxLoginAttempts * 3
	statuses := make(chan int, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Authenticate(ctx, dto.LoginRequest{Email: "race@example.com", Password: "Wrong!"})
			statuses <- statusOf(err)
		}()
	}
	wg.Wait()
	close(statuses)

	checked, locked := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests:
			locked++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if checked != maxLoginAttempts || locked != attempts-maxLoginAttempts {
		t.Fatalf("password checks = %d, locked = %d; want %d checks", checked, locked, maxLoginAttempts)
	}

	// The correct password is also refused while locked.
	if _, err := svc.Authenticate(ctx, dto.LoginRequest{Email: "race@example.com", Password: "Password1!"}); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("locked account login err = %v, want 429", err)
	}
}

// A lockout response must look the same whether or not the address exists.
func TestAuthenticate_LockoutDoesNotRevealExistence(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := context.Background()
	if _, err := svc.Register(ctx, dto.RegisterRequest{
		Email: "known@example.com", Password: "Password1!", Name: "Known",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, addr := range []string{"known@example.com", "unknown@example.com"} {
		for i := range maxLoginAttempts {
			_, err := svc.Authenticate(ctx, dto.LoginRequest{Email: addr, Password: "Wrong!"})
			if statusOf(err) != http.StatusUnauthorized {
				t.Fatalf("%s attempt %d: err = %v, want 401", addr, i+1, err)
			}
		}
		_, err := svc.Authenticate(ctx, dto.LoginRequest{Email: addr, Password: "Wrong!"})
		if statusOf(err) != http.StatusTooManyRequests {
			t.Fatalf("%s: err = %v, want 429 after %d failures", addr, err, maxLoginAttempts)
		}
	}
}

// The password is global (ADR 0004), so the lockout is too: failures in one
// tenant count against the same account in another.
func TestAuthenticate_LockoutIsGlobalAcrossTenants(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := context.Background()
	if _, err := svc.Register(ctx, dto.RegisterRequest{Email: "g@example.com", Password: "Password1!", Name: "G"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for range maxLoginAttempts {
		_, _ = svc.Authenticate(tenant.WithID(ctx, 2), dto.LoginRequest{Email: "g@example.com", Password: "Wrong!"})
	}
	if _, err := svc.Authenticate(ctx, dto.LoginRequest{Email: "g@example.com", Password: "Password1!"}); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("login in tenant 1 after failures in tenant 2: err = %v, want 429", err)
	}
}

// Registering an address clears a lockout accrued before the account existed,
// so pre-locking an unregistered address cannot lock out its future owner.
func TestRegister_ClearsPreexistingLockout(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := context.Background()
	for range maxLoginAttempts + 1 {
		_, _ = svc.Authenticate(ctx, dto.LoginRequest{Email: "future@example.com", Password: "Wrong!"})
	}
	if _, err := svc.Register(ctx, dto.RegisterRequest{
		Email: "future@example.com", Password: "Password1!", Name: "Future",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Authenticate(ctx, dto.LoginRequest{Email: "future@example.com", Password: "Password1!"}); err != nil {
		t.Fatalf("login after registration: %v", err)
	}
}

// Lockout keys must not carry the raw address (PII in the cache), yet every
// spelling of one normalized address must share its counters.
func TestLoginAttemptKeysHashTheEmail(t *testing.T) {
	ctx := WithClientIP(context.Background(), "203.0.113.7")
	source, account := loginAttemptKeys(ctx, "Victim@Example.com")
	for _, key := range []string{source, account} {
		if strings.Contains(strings.ToLower(key), "victim") || strings.Contains(key, "@") {
			t.Fatalf("lockout key %q contains the email address", key)
		}
	}
	if _, sameAccount := loginAttemptKeys(ctx, "  victim@example.com "); sameAccount != account {
		t.Fatalf("normalized spellings must share one account key: %q vs %q", sameAccount, account)
	}
}
