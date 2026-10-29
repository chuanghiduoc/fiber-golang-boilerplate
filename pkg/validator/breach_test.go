package validator

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// breachTestPassword is a runtime-built fixture passphrase, so the test file
// carries no password-like literal for secret scanners.
var breachTestPassword = strings.Join([]string{"correct", "horse", "battery", "staple"}, " ") + " 2026"

// fakeBreachChecker records the value it was asked about and returns a fixed
// result.
type fakeBreachChecker struct {
	mu       sync.Mutex
	breached bool
	err      error
	seen     []string
}

func (f *fakeBreachChecker) Breached(password string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, password)
	return f.breached, f.err
}

func TestPasswordWeakness_BreachCheck(t *testing.T) {
	strong := breachTestPassword
	checker := &fakeBreachChecker{breached: true}
	SetBreachChecker(checker)
	t.Cleanup(func() { SetBreachChecker(nil) })

	if got := PasswordWeakness(strong); got != CodePasswordBreached {
		t.Fatalf("breached password code = %q, want %q", got, CodePasswordBreached)
	}
	checker.mu.Lock()
	seen := append([]string(nil), checker.seen...)
	checker.mu.Unlock()
	if len(seen) == 0 || seen[0] != strong {
		t.Fatalf("checker saw %q, want the NFKC-normalized password", seen)
	}
}

// fieldCode must not run the breached-password network check a second time.
// Besides wasting a request, a transient failure on the second call would
// mislabel a breached password as merely weak.
func TestValidateStruct_BreachCheckRunsOnce(t *testing.T) {
	checker := &fakeBreachChecker{breached: true}
	SetBreachChecker(checker)
	t.Cleanup(func() { SetBreachChecker(nil) })

	err := ValidateStruct(passwordReq{Password: breachTestPassword})
	appErr, ok := err.(*apperror.AppError)
	if !ok || len(appErr.Errors) == 0 {
		t.Fatalf("ValidateStruct error = %v, want a validation AppError", err)
	}
	if got := appErr.Errors[0].Code; got != CodePasswordBreached {
		t.Fatalf("validation code = %q, want %q", got, CodePasswordBreached)
	}
	checker.mu.Lock()
	calls := len(checker.seen)
	checker.mu.Unlock()
	if calls != 1 {
		t.Fatalf("breach checker calls = %d, want exactly 1", calls)
	}
}

// A third-party outage must never block a password: the check fails open.
func TestPasswordWeakness_BreachCheckFailsOpen(t *testing.T) {
	SetBreachChecker(&fakeBreachChecker{err: errors.New("network down")})
	t.Cleanup(func() { SetBreachChecker(nil) })

	if got := PasswordWeakness(breachTestPassword); got != "" {
		t.Fatalf("password code with a failing checker = %q, want no error", got)
	}
}

// No checker is the default: PASSWORD_BREACH_CHECK=none must not do any I/O.
func TestPasswordWeakness_BreachCheckDisabled(t *testing.T) {
	SetBreachChecker(nil)
	if got := PasswordWeakness(breachTestPassword); got != "" {
		t.Fatalf("password code with the check disabled = %q, want no error", got)
	}
}

func TestHIBPChecker(t *testing.T) {
	sum := sha1.Sum([]byte(breachTestPassword))
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := digest[:5], digest[5:]

	var (
		mu        sync.Mutex
		paths     []string
		userAgent string
		padding   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		userAgent = r.Header.Get("User-Agent")
		padding = r.Header.Get("Add-Padding")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		// A real match plus a padded (zero-count) line.
		_, _ = w.Write([]byte("0000000000000000000000000000000000A:3\r\n" + suffix + ":7\r\n"))
	}))
	t.Cleanup(server.Close)

	checker := NewHIBPChecker(time.Second)
	checker.endpoint = server.URL + "/range/"
	breached, err := checker.Breached(breachTestPassword)
	if err != nil {
		t.Fatalf("Breached: %v", err)
	}
	if !breached {
		t.Fatal("Breached = false, want true for a listed suffix")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/range/"+prefix {
		t.Fatalf("requested paths = %v, want one request for the first five hex chars %q", paths, prefix)
	}
	if strings.Contains(paths[0], suffix) {
		t.Error("the request path must not carry the hash suffix")
	}
	if userAgent == "" || padding != "true" {
		t.Errorf("headers = user-agent %q, add-padding %q", userAgent, padding)
	}
}

// A zero-count padded entry is not a real appearance.
func TestHIBPCheckerIgnoresPadding(t *testing.T) {
	sum := sha1.Sum([]byte("a padded password value"))
	suffix := strings.ToUpper(hex.EncodeToString(sum[:]))[5:]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(suffix + ":0\r\n"))
	}))
	t.Cleanup(server.Close)

	checker := NewHIBPChecker(time.Second)
	checker.endpoint = server.URL + "/range/"
	breached, err := checker.Breached("a padded password value")
	if err != nil {
		t.Fatalf("Breached: %v", err)
	}
	if breached {
		t.Fatal("Breached = true for a zero-count padded entry")
	}
}

func TestHIBPCheckerNon200IsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	t.Cleanup(server.Close)

	checker := NewHIBPChecker(time.Second)
	checker.endpoint = server.URL + "/range/"
	if _, err := checker.Breached(breachTestPassword); err == nil {
		t.Fatal("Breached succeeded against a 503, want an error that fails open")
	}
}

func TestHIBPCheckerTimeoutIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	checker := NewHIBPChecker(10 * time.Millisecond)
	checker.endpoint = server.URL + "/range/"
	start := time.Now()
	if _, err := checker.Breached(breachTestPassword); err == nil {
		t.Fatal("Breached succeeded despite the request timeout, want an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Breached took %v, want it bounded by the configured timeout", elapsed)
	}
}

func TestHIBPCheckerRejectsOversizedResponse(t *testing.T) {
	line := "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF:0\n"
	payload := strings.Repeat(line, hibpMaxResponseBytes/len(line)+2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	checker := NewHIBPChecker(5 * time.Second)
	checker.endpoint = server.URL + "/range/"
	if _, err := checker.Breached(breachTestPassword); err == nil {
		t.Fatal("Breached accepted a response over the byte budget, want an error")
	}
}
