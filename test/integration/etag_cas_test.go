//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// TestETag_ConcurrentWritersHaveExactlyOneWinner proves the compare-and-swap
// is in the UPDATE: both writers read the same ETag, both send it, and the
// repository's conditional UPDATE lets exactly one row change.
func TestETag_ConcurrentWritersHaveExactlyOneWinner(t *testing.T) {
	full := newIntegrationApp(t, "test")
	ctx := context.Background()
	admin := testutil.CreateMember(t, ctx, full.pool, tenant.DefaultID, "cas-admin@example.com",
		testutil.MemberOptions{Role: identitydto.RoleAdmin})
	token := testutil.SessionToken(t, ctx, full.pool, admin, full.config.JWT.Secret)

	do := func(t *testing.T, method, path, body string, headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		var reader io.Reader = http.NoBody
		if body != "" {
			reader = bytes.NewReader([]byte(body))
		}
		req, err := http.NewRequest(method, path, reader)
		require.NoError(t, err)
		if body != "" {
			contentType := "application/json"
			if method == http.MethodPatch {
				contentType = "application/merge-patch+json"
			}
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Tenant-Slug", "default")
		if method != http.MethodGet {
			req.Header.Set("Idempotency-Key", uuid.NewString())
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := full.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		payload, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, payload
	}

	resp, body := do(t, http.MethodPost, "/api/v1/roles",
		`{"key":"cas_role","name":"CAS role","permissions":["profile:read"]}`, nil)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode, "body: %s", body)
	var created identitydto.RoleResponse
	require.NoError(t, json.Unmarshal(body, &created))

	resp, body = do(t, http.MethodGet, "/api/v1/roles/"+created.ID.String(), "", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)

	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			resp, body := do(t, http.MethodPatch, "/api/v1/roles/"+created.ID.String(),
				`{"name":"`+name+`"}`, map[string]string{"If-Match": etag})
			results <- result{status: resp.StatusCode, body: body}
		}(name)
	}
	wg.Wait()
	close(results)

	var ok, failed int
	for res := range results {
		switch res.status {
		case fiber.StatusOK:
			ok++
		case fiber.StatusPreconditionFailed:
			failed++
			assert.Contains(t, string(res.body), "resource.precondition_failed")
		default:
			t.Fatalf("unexpected status %d: %s", res.status, res.body)
		}
	}
	assert.Equal(t, 1, ok, "exactly one concurrent writer wins")
	assert.Equal(t, 1, failed, "the loser gets 412, not a silent overwrite")
}
