package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunHealthcheckReportsStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "ready", status: http.StatusOK, wantCode: 0, wantStdout: "ok: "},
		{name: "not ready", status: http.StatusServiceUnavailable, wantCode: 1, wantStderr: "returned HTTP 503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			var stdout, stderr bytes.Buffer
			code := runHealthcheck([]string{"--url", srv.URL}, &stdout, &stderr, func(string) string { return "" })

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunHealthcheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	var stdout, stderr bytes.Buffer
	code := runHealthcheck([]string{"--url", url, "--timeout", "1s"}, &stdout, &stderr, func(string) string { return "" })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "healthcheck:") {
		t.Errorf("stderr = %q, want a healthcheck error", stderr.String())
	}
}

func TestRunHealthcheckUsesAppPort(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The default URL is built from APP_PORT, not from a flag: verify the
	// environment lookup without needing a listener on that port.
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	var stdout, stderr bytes.Buffer
	code := runHealthcheck(nil, &stdout, &stderr, func(key string) string {
		if key == "APP_PORT" {
			return port
		}
		return ""
	})

	// httptest listens on 127.0.0.1, and the default URL is 127.0.0.1, so this
	// succeeds and proves APP_PORT was used.
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if gotPath != "/readyz" {
		t.Errorf("probed path = %q, want /readyz", gotPath)
	}
}

func TestRunHealthcheckHelpAndUsageErrors(t *testing.T) {
	t.Run("help exits zero", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"--help"}, &stdout, &stderr, func(string) string { return "" }); code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if !strings.Contains(stdout.String(), "Usage: healthcheck") {
			t.Errorf("stdout = %q, want usage text", stdout.String())
		}
	})

	t.Run("unknown flag exits two", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"--nope"}, &stdout, &stderr, func(string) string { return "" }); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
	})

	t.Run("positional argument exits two", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"extra"}, &stdout, &stderr, func(string) string { return "" }); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
	})

	t.Run("non-positive timeout exits two", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runHealthcheck([]string{"--timeout", "0s"}, &stdout, &stderr, func(string) string { return "" }); code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
	})
}
