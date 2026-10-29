package validator

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // HIBP range lookup requires SHA-1: the API is indexed by it and only a 5-char prefix leaves the process
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// BreachChecker reports whether a password appears in a public breach corpus.
// It is an optional capability: a nil checker disables the check, so a
// deployment that cannot reach the provider never blocks registration.
type BreachChecker interface {
	// Breached returns true when the password is known to have been exposed.
	// An error means the check could not be completed; callers fail open.
	Breached(password string) (bool, error)
}

var (
	breachMu      sync.RWMutex
	breachChecker BreachChecker
)

// SetBreachChecker installs the process-wide breached-password checker. It is
// called once at startup from PASSWORD_BREACH_CHECK; passing nil disables the
// check.
func SetBreachChecker(checker BreachChecker) {
	breachMu.Lock()
	defer breachMu.Unlock()
	breachChecker = checker
}

// currentBreachChecker returns the installed checker, if any.
func currentBreachChecker() BreachChecker {
	breachMu.RLock()
	defer breachMu.RUnlock()
	return breachChecker
}

// isBreached consults the optional checker and fails open: a third party's
// outage must never block account creation or a password change. The failure
// is logged without the password or its hash.
func isBreached(password string) bool {
	checker := currentBreachChecker()
	if checker == nil {
		return false
	}
	breached, err := checker.Breached(password)
	if err != nil {
		warnBreachCheckFailure(err)
		return false
	}
	return breached
}

// warnBreachCheckFailure logs a failed optional check. It is a warning, not an
// error: the password is allowed through, and the cause is kept to the error
// value (which never contains the password).
func warnBreachCheckFailure(err error) {
	slog.Warn("breached-password check unavailable; allowing the password",
		slog.Any("error", err))
}

const (
	hibpRangeURL = "https://api.pwnedpasswords.com/range/"
	// hibpMaxResponseBytes bounds the range response (HIBP returns a few
	// hundred KB at most); a truncated read fails the check, which fails open.
	hibpMaxResponseBytes = 8 << 20
	hibpUserAgent        = "fiber-golang-boilerplate password policy"
)

// HIBPChecker checks passwords with the Have I Been Pwned range API using
// k-anonymity: only the first five hex characters of the password's SHA-1 are
// sent, and the matching suffix is compared locally. The provider therefore
// never receives the password or enough of its hash to recover it.
type HIBPChecker struct {
	client   *http.Client
	endpoint string
}

// NewHIBPChecker builds a checker whose requests time out after timeout. A
// non-positive timeout falls back to the documented two-second budget.
func NewHIBPChecker(timeout time.Duration) *HIBPChecker {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HIBPChecker{
		client:   &http.Client{Timeout: timeout},
		endpoint: hibpRangeURL,
	}
}

// Breached implements BreachChecker. The password is hashed with SHA-1 exactly
// as HIBP stores it; normalization happens before this call so the value
// checked matches the value that will be hashed for storage.
func (h *HIBPChecker) Breached(password string) (bool, error) {
	sum := sha1.Sum([]byte(password)) //nolint:gosec // HIBP range lookup is indexed by SHA-1; it is a lookup key, not a security primitive
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	req, err := http.NewRequest(http.MethodGet, h.endpoint+digest[:5], http.NoBody)
	if err != nil {
		return false, fmt.Errorf("build HIBP range request: %w", err)
	}
	// Add-Padding asks for randomly padded entries, so response size does not
	// reveal whether the prefix has any real matches.
	req.Header.Set("Add-Padding", "true")
	req.Header.Set("User-Agent", hibpUserAgent)

	resp, err := h.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("HIBP range request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return false, fmt.Errorf("HIBP range API returned status %d", resp.StatusCode)
	}

	suffix := digest[5:]
	// Read one byte past the budget so an oversized response is detectable.
	// A plain LimitReader reports EOF at the limit, which would make a
	// truncated response look like a clean password.
	limited := &io.LimitedReader{R: resp.Body, N: hibpMaxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	for scanner.Scan() {
		lineSuffix, count, ok := strings.Cut(strings.TrimSpace(scanner.Text()), ":")
		if !ok || !strings.EqualFold(lineSuffix, suffix) {
			continue
		}
		// Padded entries carry a zero count and are not real appearances.
		return count != "0", nil
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read HIBP range response: %w", err)
	}
	if limited.N == 0 {
		return false, fmt.Errorf("read HIBP range response: exceeds %d bytes", hibpMaxResponseBytes)
	}
	return false, nil
}
