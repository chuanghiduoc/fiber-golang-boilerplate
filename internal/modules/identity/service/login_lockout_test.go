package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return resolvedStatus(appErr)
	}
	return -1
}

func newTestUserService(db *memDB) UserService {
	return NewUserService(UserDeps{Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache()})
}

// Parallel wrong-password attempts must not all pass a stale "not locked yet"
// read: the counter is reserved atomically before the Argon2id comparison.
func TestAuthenticate_ConcurrentAttemptsCannotExceedLockout(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := testTenantCtx()
	if _, err := svc.Register(ctx, RegisterInput{
		Email: "race@example.com", Password: testPassphrase, Name: "Race",
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
			_, err := svc.Authenticate(ctx, LoginInput{Email: "race@example.com", Password: testWrongPassword})
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
	if _, err := svc.Authenticate(ctx, LoginInput{Email: "race@example.com", Password: testPassphrase}); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("locked account login err = %v, want 429", err)
	}
}

// A lockout response must look the same whether or not the address exists.
func TestAuthenticate_LockoutDoesNotRevealExistence(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := testTenantCtx()
	if _, err := svc.Register(ctx, RegisterInput{
		Email: "known@example.com", Password: testPassphrase, Name: "Known",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, addr := range []string{"known@example.com", "unknown@example.com"} {
		for i := range maxLoginAttempts {
			_, err := svc.Authenticate(ctx, LoginInput{Email: addr, Password: testWrongPassword})
			if statusOf(err) != http.StatusUnauthorized {
				t.Fatalf("%s attempt %d: err = %v, want 401", addr, i+1, err)
			}
		}
		_, err := svc.Authenticate(ctx, LoginInput{Email: addr, Password: testWrongPassword})
		if statusOf(err) != http.StatusTooManyRequests {
			t.Fatalf("%s: err = %v, want 429 after %d failures", addr, err, maxLoginAttempts)
		}
	}
}

// The password is global, so the lockout is too: failures in one
// tenant count against the same account in another.
func TestAuthenticate_LockoutIsGlobalAcrossTenants(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := testTenantCtx()
	if _, err := svc.Register(ctx, RegisterInput{Email: "g@example.com", Password: testPassphrase, Name: "G"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for range maxLoginAttempts {
		_, _ = svc.Authenticate(tenant.WithID(ctx, testID(2)), LoginInput{Email: "g@example.com", Password: testWrongPassword}) // ggignore: test fixture, not a credential
	}
	if _, err := svc.Authenticate(ctx, LoginInput{Email: "g@example.com", Password: testPassphrase}); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("login in tenant 1 after failures in tenant 2: err = %v, want 429", err)
	}
}

// Registering an address clears a lockout accrued before the account existed,
// so pre-locking an unregistered address cannot lock out its future owner.
func TestRegister_ClearsPreexistingLockout(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := testTenantCtx()
	for range maxLoginAttempts + 1 {
		_, _ = svc.Authenticate(ctx, LoginInput{Email: "future@example.com", Password: testWrongPassword})
	}
	if _, err := svc.Register(ctx, RegisterInput{
		Email: "future@example.com", Password: testPassphrase, Name: "Future",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Authenticate(ctx, LoginInput{Email: "future@example.com", Password: testPassphrase}); err != nil {
		t.Fatalf("login after registration: %v", err)
	}
}

// Five sources (25 failures) must no longer lock the account: NIST SP 800-63B
// rev4 caps the account-wide budget at 100 consecutive failures.
func TestAuthenticate_FiveSourcesDoNotLockTheAccount(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := testTenantCtx()
	const email = "budget@example.com"
	if _, err := svc.Register(ctx, RegisterInput{
		Email: email, Password: testPassphrase, Name: "Budget",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for i := range 5 {
		source := WithClientIP(ctx, fmt.Sprintf("203.0.113.%d", i))
		for range maxLoginAttempts {
			if _, err := svc.Authenticate(source, LoginInput{Email: email, Password: testWrongPassword}); statusOf(err) != http.StatusUnauthorized {
				t.Fatalf("source %d failure: err = %v, want 401", i, err)
			}
		}
	}

	// A correct password from a fresh source still works: the old 5x5 budget
	// would have locked the account after the fifth source.
	if _, err := svc.Authenticate(WithClientIP(ctx, "198.51.100.1"), LoginInput{Email: email, Password: testPassphrase}); err != nil {
		t.Fatalf("correct password after 25 distributed failures: %v", err)
	}
}

// The account-wide counter rejects the 101st failure and then refuses even the
// correct password until the window expires. One source stops contributing
// after its own budget, so reaching the ceiling takes 20 addresses.
func TestAuthenticate_AccountBudgetCapsAtOneHundred(t *testing.T) {
	svc := newTestUserService(newMemDB())
	ctx := testTenantCtx()
	const email = "ceiling@example.com"
	if _, err := svc.Register(ctx, RegisterInput{
		Email: email, Password: testPassphrase, Name: "Ceiling",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for i := range maxAccountLoginFailures / maxLoginAttempts {
		source := WithClientIP(ctx, fmt.Sprintf("203.0.113.%d", i))
		for range maxLoginAttempts {
			if _, err := svc.Authenticate(source, LoginInput{Email: email, Password: testWrongPassword}); statusOf(err) != http.StatusUnauthorized {
				t.Fatalf("source %d failure: err = %v, want 401", i, err)
			}
		}
	}
	if _, err := svc.Authenticate(WithClientIP(ctx, "198.51.100.2"), LoginInput{Email: email, Password: testWrongPassword}); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("first failure past the account budget: err = %v, want 429", err)
	}
	if _, err := svc.Authenticate(WithClientIP(ctx, "198.51.100.3"), LoginInput{Email: email, Password: testPassphrase}); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("correct password while the account is locked: err = %v, want 429", err)
	}
}

// A correct password clears only its own source counter. The account-wide
// counter keeps counting failures from other addresses until it expires.
func TestAuthenticate_SuccessClearsOnlyTheSourceCounter(t *testing.T) {
	db := newMemDB()
	store := cache.NewMemoryCache()
	svc := NewUserService(UserDeps{Stores: db.stores(), Tx: memTx{db}, Cache: store})
	ctx := testTenantCtx()
	const email = "source-only@example.com"
	if _, err := svc.Register(ctx, RegisterInput{
		Email: email, Password: testPassphrase, Name: "Source",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	source := WithClientIP(ctx, "203.0.113.10")
	for range 3 {
		if _, err := svc.Authenticate(source, LoginInput{Email: email, Password: testWrongPassword}); statusOf(err) != http.StatusUnauthorized {
			t.Fatalf("failure: err = %v, want 401", err)
		}
	}
	if _, err := svc.Authenticate(source, LoginInput{Email: email, Password: testPassphrase}); err != nil {
		t.Fatalf("correct password: %v", err)
	}

	sourceKey, accountKey := loginAttemptKeys(source, email)
	exists, err := store.Exists(ctx, sourceKey)
	if err != nil {
		t.Fatalf("source counter: %v", err)
	}
	if exists {
		t.Error("a correct password must clear the per-source counter")
	}
	exists, err = store.Exists(ctx, accountKey)
	if err != nil {
		t.Fatalf("account counter: %v", err)
	}
	if !exists {
		t.Error("a correct password must not clear the account-wide counter")
	}
}

// Lockout keys must not carry the raw address (PII in the cache), yet every
// spelling of one normalized address must share its counters.
func TestLoginAttemptKeysHashTheEmail(t *testing.T) {
	ctx := WithClientIP(testTenantCtx(), "203.0.113.7")
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
