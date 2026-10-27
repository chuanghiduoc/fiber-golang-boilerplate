package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scrape renders the current Prometheus registry exactly as the /metrics
// endpoint does.
func scrape(t *testing.T) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	require.Equal(t, http.StatusOK, recorder.Code)
	return recorder.Body.String()
}

func TestHTTPServerRequestDurationFollowsSemconv(t *testing.T) {
	provider, err := Setup(Options{ServiceVersion: "1.2.3"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	RecordHTTPServerRequest(context.Background(), "GET", "/users/:id", 200, "", 0.012)
	RecordHTTPServerRequest(context.Background(), "POST", "/users/:id/activate", 500, "500", 0.2)
	RecordHTTPServerRequest(context.Background(), "BREW", "", 404, "404", 0.001)

	body := scrape(t)

	assert.Contains(t, body, "# TYPE http_server_request_duration_seconds histogram")
	assert.Contains(t, body, "http_server_request_duration_seconds_bucket{")
	assert.Contains(t, body, "http_server_request_duration_seconds_sum{")
	assert.Contains(t, body, "http_server_request_duration_seconds_count{")
	assert.Contains(t, body, `http_request_method="GET"`)
	assert.Contains(t, body, `http_route="/users/:id"`)
	assert.Contains(t, body, `http_response_status_code="200"`)
	assert.Contains(t, body, `error_type="500"`)
	assert.Contains(t, body, `le="0.005"`)
	assert.Contains(t, body, `le="10"`)

	// The separate request counter is gone: the histogram _count replaces it.
	assert.NotContains(t, body, "http_requests_total")
	assert.NotContains(t, body, "http_request_duration_seconds")

	// The build version is the resource service.version.
	assert.Contains(t, body, `service_version="1.2.3"`)
}

func TestDomainMetricsKeepPrometheusNames(t *testing.T) {
	provider, err := Setup(Options{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	JobsClaimedTotal.WithLabelValues("email.send").Inc()
	JobsQueueLagSeconds.WithLabelValues("email.send").Observe(1.5)
	SchedulerLeader.Set(1)
	FeatureFlagEvaluationsTotal.WithLabelValues("checkout", "default", "true").Inc()

	body := scrape(t)

	for _, name := range []string{
		"jobs_claimed_total",
		"jobs_queue_lag_seconds_bucket",
		"scheduler_leader",
		"feature_flag_evaluations_total",
	} {
		assert.True(t, strings.Contains(body, name), "exposition must contain %s:\n%s", name, body)
	}
}
