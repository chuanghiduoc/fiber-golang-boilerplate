package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRequestTimeout   = 10 * time.Second
	defaultConnectTimeout   = 5 * time.Second
	defaultMaxResponseBytes = 4096
)

// ClientConfig configures a Client. The Guard is required; a nil Guard uses an
// empty allowlist, which still blocks private/loopback/link-local targets.
type ClientConfig struct {
	Guard            *Guard
	Timeout          time.Duration
	ConnectTimeout   time.Duration
	MaxResponseBytes int64
}

// Result is the terminal HTTP response of one delivery attempt. Redirects are
// never followed, so a 3xx Result is recorded and classified as permanent.
type Result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Client performs SSRF-safe outbound webhook requests. It never follows
// redirects, never consults HTTP_PROXY, and applies an overall timeout.
type Client struct {
	http             *http.Client
	guard            *Guard
	maxResponseBytes int64
}

// NewClient builds a Client. Invalid configuration is normalized to safe
// defaults rather than failing closed at request time.
func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}
	maxBytes := cfg.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseBytes
	}
	guard := cfg.Guard
	if guard == nil {
		guard, _ = NewGuard(nil)
	}

	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           guard.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A 3xx is a terminal delivery outcome, not a navigation hint.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		guard:            guard,
		maxResponseBytes: maxBytes,
	}
}

// Do sends body to rawURL with headers. URL and DNS/address validation happen
// before any connection is made, and the connection is pinned to a vetted IP.
func (c *Client) Do(ctx context.Context, rawURL string, headers http.Header, body []byte) (*Result, error) {
	if c == nil || c.http == nil {
		return nil, fmt.Errorf("%w: client is nil", ErrInvalidTarget)
	}
	if _, err := c.guard.ValidateURL(rawURL); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", UserAgent)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	data, readErr := io.ReadAll(limited)
	// Drain a bounded remainder so the connection can be reused; never let a
	// receiver stream an unbounded body into this process.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("webhook: read response: %w", readErr)
	}
	if int64(len(data)) > c.maxResponseBytes {
		data = data[:c.maxResponseBytes]
	}
	return &Result{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: data}, nil
}

// CloseIdleConnections releases pooled connections. Workers call it on
// shutdown; the client remains usable afterwards.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.http == nil {
		return
	}
	c.http.CloseIdleConnections()
}

// Outcome is the retry disposition of one delivery attempt.
type Outcome int

const (
	// OutcomeSuccess means the receiver accepted the delivery (2xx).
	OutcomeSuccess Outcome = iota
	// OutcomeRetry means a later attempt may succeed.
	OutcomeRetry
	// OutcomePermanent means the delivery must be marked dead immediately.
	OutcomePermanent
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeRetry:
		return "retry"
	default:
		return "permanent"
	}
}

// ClassifyStatus maps an HTTP status to a delivery outcome.
//
//	2xx -> success
//	3xx -> permanent (redirects are never followed)
//	4xx -> permanent, except 408, 425, and 429 which are retryable
//	5xx -> retryable
//
// Codes outside 200-599 are permanent because a malformed response cannot be
// fixed by redelivery.
func ClassifyStatus(status int) Outcome {
	switch {
	case status >= 200 && status < 300:
		return OutcomeSuccess
	case status >= 300 && status < 400:
		return OutcomePermanent
	case status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests:
		return OutcomeRetry
	case status >= 400 && status < 500:
		return OutcomePermanent
	case status >= 500 && status < 600:
		return OutcomeRetry
	default:
		return OutcomePermanent
	}
}

// ParseRetryAfter parses a Retry-After header value, either delay-seconds or an
// HTTP date. A value in the past yields zero. The boolean reports whether a
// valid value was present.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		const maxSeconds = int64(24 * time.Hour / time.Second)
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

// Backoff returns the deterministic exponential delay before attempt+1:
// base, 2*base, 4*base, ... capped at maxDelay. attempt is the 1-based attempt
// that just failed.
func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maxDelay <= 0 {
		maxDelay = time.Hour
	}
	if maxDelay < base {
		maxDelay = base
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= maxDelay || delay > maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

// RetryDelay selects the delay before the next delivery attempt. A valid
// Retry-After can only lengthen the exponential backoff, never shorten it, so
// "Retry-After: 0" or a past date cannot make a receiver hammer itself with
// immediate retries. It is capped at maxDelay; the boolean reports whether
// Retry-After was used.
func RetryDelay(attempt int, base, maxDelay time.Duration, retryAfter string, now time.Time) (time.Duration, bool) {
	backoff := Backoff(attempt, base, maxDelay)
	if delay, ok := ParseRetryAfter(retryAfter, now); ok {
		if maxDelay > 0 && delay > maxDelay {
			delay = maxDelay
		}
		if delay < backoff {
			delay = backoff
		}
		return delay, true
	}
	return backoff, false
}

// TransportError is a non-HTTP failure. Blocked and invalid targets are
// permanent; timeouts, DNS failures, and connection errors are retryable.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "webhook: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// ClassifyError maps a Do error to an outcome. A permanent target error is
// dead on arrival; everything else (including context cancellation from a
// worker timeout) is retryable while attempts remain.
func ClassifyError(err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	if IsBlocked(err) || IsInvalidTarget(err) || errors.Is(err, ErrEmptySecret) {
		return OutcomePermanent
	}
	return OutcomeRetry
}

// StatusError builds the recorded error text for a non-2xx response without
// including the response body, which could contain attacker-controlled data.
func StatusError(status int) string {
	return fmt.Sprintf("receiver returned HTTP %d", status)
}
