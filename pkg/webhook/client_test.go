package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func loopbackClient(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	// httptest servers listen on random ports; the guard's port policy is
	// covered by TestGuard_PortPolicy, so these client tests allow every port.
	ports := make([]int, 0, 65535)
	for port := 1; port <= 65535; port++ {
		ports = append(ports, port)
	}
	guard, err := NewGuardWithPorts([]string{"127.0.0.1", "::1", "localhost"}, ports)
	if err != nil {
		t.Fatalf("NewGuardWithPorts: %v", err)
	}
	return NewClient(ClientConfig{Guard: guard, Timeout: timeout, ConnectTimeout: time.Second})
}

func TestClient_ConnectTimeoutReachesGuardDialer(t *testing.T) {
	guard, err := NewGuard(nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	const connect = 750 * time.Millisecond
	client := NewClient(ClientConfig{Guard: guard, ConnectTimeout: connect})

	if client.guard.dialTimeout != connect {
		t.Fatalf("guard dial timeout = %v, want configured %v", client.guard.dialTimeout, connect)
	}
	if guard.dialTimeout != defaultConnectTimeout {
		t.Fatalf("caller's guard was mutated: dial timeout = %v", guard.dialTimeout)
	}
}

func TestClient_RejectsPrivateTargetWithoutAllowlist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	guard, err := NewGuardWithPorts(nil, []int{port})
	if err != nil {
		t.Fatalf("NewGuardWithPorts: %v", err)
	}
	client := NewClient(ClientConfig{Guard: guard, Timeout: time.Second})

	_, err = client.Do(context.Background(), server.URL, nil, []byte(`{}`))
	if !errors.Is(err, ErrBlockedTarget) {
		t.Fatalf("Do() = %v, want ErrBlockedTarget", err)
	}
}

func TestClient_PostsSignedRequestAndDoesNotFollowRedirects(t *testing.T) {
	var gotBody string
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := loopbackClient(t, 2*time.Second)
	headers := http.Header{}
	headers.Set(HeaderID, "evt_1")
	headers.Set(HeaderTimestamp, "123")
	result, err := client.Do(context.Background(), server.URL, headers, []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("Do(): %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", result.StatusCode)
	}
	if gotHeaders.Get(HeaderID) != "evt_1" || gotHeaders.Get(HeaderTimestamp) != "123" {
		t.Fatalf("headers not forwarded: %v", gotHeaders)
	}
	if gotHeaders.Get("User-Agent") != UserAgent {
		t.Errorf("User-Agent = %q, want %q", gotHeaders.Get("User-Agent"), UserAgent)
	}
	if gotBody != `{"hello":"world"}` {
		t.Errorf("body = %q", gotBody)
	}

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer redirect.Close()
	result, err = client.Do(context.Background(), redirect.URL, nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("redirect Do(): %v", err)
	}
	if result.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302 (must not follow)", result.StatusCode)
	}
	if got := ClassifyStatus(result.StatusCode); got != OutcomePermanent {
		t.Fatalf("classify 302 = %s, want permanent", got)
	}
}

func TestClient_TimeoutIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := loopbackClient(t, 50*time.Millisecond)
	_, err := client.Do(context.Background(), server.URL, nil, []byte(`{}`))
	if err == nil {
		t.Fatal("Do() should time out")
	}
	if IsBlocked(err) {
		t.Fatalf("timeout must not be classified as blocked: %v", err)
	}
	if got := ClassifyError(err); got != OutcomeRetry {
		t.Fatalf("timeout classification = %s, want retry", got)
	}
}

func TestClient_InvalidSchemeIsPermanent(t *testing.T) {
	client := loopbackClient(t, time.Second)
	_, err := client.Do(context.Background(), "file:///etc/passwd", nil, nil)
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("Do(file://) = %v, want ErrInvalidTarget", err)
	}
	if got := ClassifyError(err); got != OutcomePermanent {
		t.Fatalf("classification = %s, want permanent", got)
	}
}
