package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestGuard_BlocksNonPublicAddresses(t *testing.T) {
	guard, err := NewGuard(nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}

	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "169.254.1.1", "0.0.0.0", "0.1.2.3",
		"100.64.0.1", "192.0.0.10", "198.18.0.1", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "255.255.255.255", "240.0.0.1",
		"::1", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1",
		"2001:db8::1", "::ffff:127.0.0.1",
	}
	for _, raw := range blocked {
		t.Run(raw, func(t *testing.T) {
			ip := net.ParseIP(raw)
			if ip == nil {
				t.Fatalf("invalid test IP %q", raw)
			}
			if err := guard.CheckIP(ip); !errors.Is(err, ErrBlockedTarget) {
				t.Fatalf("CheckIP(%s) = %v, want ErrBlockedTarget", raw, err)
			}
		})
	}

	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, raw := range public {
		ip := net.ParseIP(raw)
		if err := guard.CheckIP(ip); err != nil {
			t.Errorf("CheckIP(%s) rejected public address: %v", raw, err)
		}
	}
}

func TestIsBlockedIP_IPv6Transition(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		// Well-known NAT64: only as public as the embedded IPv4 address.
		{"64:ff9b::7f00:1", true},       // 127.0.0.1
		{"64:ff9b::a9fe:a9fe", true},    // 169.254.169.254
		{"64:ff9b::a00:1", true},        // 10.0.0.1
		{"64:ff9b::808:808", false},     // 8.8.8.8
		{"64:ff9b:1::808:808", true},    // local-use NAT64
		{"2002:7f00:1::1", true},        // 6to4 of 127.0.0.1
		{"2002:c0a8:101::1", true},      // 6to4 of 192.168.1.1
		{"2002:808:808::1", false},      // 6to4 of 8.8.8.8
		{"::7f00:1", true},              // IPv4-compatible 127.0.0.1
		{"::a00:1", true},               // IPv4-compatible 10.0.0.1
		{"::808:808", false},            // IPv4-compatible 8.8.8.8
		{"::ffff:0:7f00:1", true},       // IPv4-translated 127.0.0.1
		{"::ffff:0:808:808", false},     // IPv4-translated 8.8.8.8
		{"fec0::1", true},               // site-local
		{"feff::1", true},               // top of site-local /10
		{"2001::1", true},               // Teredo
		{"2001:0:4136:e378::1", true},   // Teredo
		{"2001:4860:4860::8888", false}, // public, outside Teredo /32
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("invalid test IP %q", tc.ip)
			}
			if got := isBlockedIP(ip); got != tc.blocked {
				t.Fatalf("isBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

func TestGuard_Allowlist(t *testing.T) {
	guard, err := NewGuard([]string{"127.0.0.1", "10.10.0.0/16", "internal.example"})
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}

	for _, raw := range []string{"127.0.0.1", "10.10.4.5"} {
		if err := guard.CheckIP(net.ParseIP(raw)); err != nil {
			t.Errorf("allowlisted %s rejected: %v", raw, err)
		}
	}
	if err := guard.CheckIP(net.ParseIP("10.11.0.1")); !errors.Is(err, ErrBlockedTarget) {
		t.Errorf("non-allowlisted private IP accepted: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := guard.CheckHost(ctx, "localhost")
	if err != nil {
		t.Fatalf("allowlisted localhost rejected: %v", err)
	}
	if len(ips) == 0 {
		t.Fatal("localhost resolved to no addresses")
	}
	for _, ip := range ips {
		if err := guard.CheckIP(ip); err != nil {
			t.Fatalf("resolved localhost address %s should be allowed: %v", ip, err)
		}
	}
}

func TestGuard_InvalidAllowlist(t *testing.T) {
	for _, entry := range []string{"10.0.0.0/99", "http://host", "bad host"} {
		if _, err := NewGuard([]string{entry}); err == nil {
			t.Errorf("NewGuard(%q) should fail", entry)
		}
	}
}

func TestGuard_ValidateURL(t *testing.T) {
	guard, _ := NewGuard(nil)
	valid := []string{"https://example.com/hook", "http://example.com:8080/hook?x=1"}
	for _, raw := range valid {
		if _, err := guard.ValidateURL(raw); err != nil {
			t.Errorf("ValidateURL(%q) rejected: %v", raw, err)
		}
	}

	invalid := []string{"", "ftp://example.com", "example.com/hook", "https://", "https://user:pass@example.com"}
	for _, raw := range invalid {
		if _, err := guard.ValidateURL(raw); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("ValidateURL(%q) error = %v, want ErrInvalidTarget", raw, err)
		}
	}
}

func TestGuard_CheckHost_BlocksLocalhostWithoutAllowlist(t *testing.T) {
	guard, _ := NewGuard(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := guard.CheckHost(ctx, "localhost")
	if !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("CheckHost(localhost) error = %v, want ErrBlockedTarget", err)
	}

	// An IP literal must be checked without DNS.
	_, err = guard.CheckHost(ctx, "127.0.0.1")
	if !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("CheckHost(127.0.0.1) error = %v, want ErrBlockedTarget", err)
	}
}

func TestSign_DeterministicForUnixSeconds(t *testing.T) {
	secret := []byte("s")
	ts := time.Unix(1_792_843_200, 900_000_000)
	other := time.Unix(1_792_843_200, 100_000_000)
	if Sign(secret, "evt", "a.b", ts, []byte("x")) != Sign(secret, "evt", "a.b", other, []byte("x")) {
		t.Error("Sign must derive only from Unix seconds")
	}
}

func TestRetryClassification(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{
		{200, OutcomeSuccess},
		{204, OutcomeSuccess},
		{301, OutcomePermanent},
		{302, OutcomePermanent},
		{400, OutcomePermanent},
		{401, OutcomePermanent},
		{404, OutcomePermanent},
		{408, OutcomeRetry},
		{425, OutcomeRetry},
		{429, OutcomeRetry},
		{500, OutcomeRetry},
		{502, OutcomeRetry},
		{503, OutcomeRetry},
		{0, OutcomePermanent},
		{100, OutcomePermanent},
		{600, OutcomePermanent},
	}
	for _, tc := range cases {
		if got := ClassifyStatus(tc.status); got != tc.want {
			t.Errorf("ClassifyStatus(%d) = %s, want %s", tc.status, got, tc.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	base := 5 * time.Second
	maxDelay := time.Minute
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, expected := range want {
		if got := Backoff(i+1, base, maxDelay); got != expected {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, expected)
		}
	}
	if got := Backoff(0, base, maxDelay); got != base {
		t.Errorf("Backoff(0) = %s, want base %s", got, base)
	}
}

func TestRetryDelay_UsesRetryAfter(t *testing.T) {
	now := time.Now().UTC()
	base := time.Second
	maxDelay := time.Hour

	delay, used := RetryDelay(1, base, maxDelay, "120", now)
	if !used || delay != 120*time.Second {
		t.Fatalf("RetryDelay(Retry-After seconds) = %s, used=%v", delay, used)
	}

	httpDate := now.Add(45 * time.Second).UTC().Format(http.TimeFormat)
	delay, used = RetryDelay(1, base, maxDelay, httpDate, now)
	if !used || delay < 40*time.Second || delay > 46*time.Second {
		t.Fatalf("RetryDelay(Retry-After date) = %s, used=%v", delay, used)
	}

	// Retry-After is capped so a hostile receiver cannot park a delivery forever.
	delay, used = RetryDelay(1, base, maxDelay, "86400", now)
	if !used || delay != maxDelay {
		t.Fatalf("RetryDelay cap = %s, used=%v", delay, used)
	}

	delay, used = RetryDelay(3, base, maxDelay, "garbage", now)
	if used || delay != 4*base {
		t.Fatalf("invalid Retry-After fallback = %s, used=%v", delay, used)
	}

	// A zero or past Retry-After never shortens the backoff schedule.
	delay, used = RetryDelay(3, base, maxDelay, "0", now)
	if !used || delay != 4*base {
		t.Fatalf("RetryDelay(Retry-After 0) = %s, used=%v, want %s", delay, used, 4*base)
	}
	pastDate := now.Add(-time.Hour).UTC().Format(http.TimeFormat)
	delay, used = RetryDelay(1, base, maxDelay, pastDate, now)
	if !used || delay != base {
		t.Fatalf("RetryDelay(past Retry-After) = %s, used=%v, want %s", delay, used, base)
	}
}

func TestClassifyError(t *testing.T) {
	if got := ClassifyError(nil); got != OutcomeSuccess {
		t.Errorf("nil error = %s, want success", got)
	}
	if got := ClassifyError(ErrBlockedTarget); got != OutcomePermanent {
		t.Errorf("blocked = %s, want permanent", got)
	}
	if got := ClassifyError(ErrInvalidTarget); got != OutcomePermanent {
		t.Errorf("invalid target = %s, want permanent", got)
	}
	if got := ClassifyError(errors.New("dial timeout")); got != OutcomeRetry {
		t.Errorf("transport error = %s, want retry", got)
	}
}
