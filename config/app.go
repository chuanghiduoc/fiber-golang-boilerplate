package config

import (
	"fmt"
	"net/url"
	"time"
)

// APP_ENV values. Only local and test may use the public sample secrets;
// production additionally enables the production safety rails.
const (
	envLocal       = "local"
	envTest        = "test"
	envDevelopment = "development"
	envStaging     = "staging"
	envProduction  = "production"
)

// maxTCPPort is the largest valid TCP port for APP_PORT and SMTP_PORT.
const maxTCPPort = 65535

// minMetricsTokenLength is the shortest METRICS_AUTH_TOKEN accepted in
// production.
const minMetricsTokenLength = 16

// APP_ROLE values: what one process runs.
const (
	// RoleAll serves the API and runs the job worker and scheduler.
	RoleAll = "all"
	// RoleAPI serves the API only; jobs are enqueued but not processed.
	RoleAPI = "api"
	// RoleWorker runs the job worker and scheduler plus health and metrics
	// endpoints, without API routes.
	RoleWorker = "worker"
)

type AppConfig struct {
	Port int    `env:"APP_PORT" envDefault:"8080"`
	Env  string `env:"APP_ENV" envDefault:"local"`
	// Role selects what the process runs (api, worker, or all). Split roles
	// let API and worker replicas scale and fail independently.
	Role string `env:"APP_ROLE" envDefault:"all"`
	// BodyLimit must be larger than STORAGE_MAX_FILE_SIZE because multipart
	// framing adds overhead before the upload handler sees the file.
	BodyLimit int `env:"APP_BODY_LIMIT" envDefault:"12582912"` // 12MB
	// JSONBodyLimit caps every request body (JSON, form, raw, and multipart
	// outside the upload route). Only the files upload route may use the larger
	// APP_BODY_LIMIT for multipart/form-data.
	JSONBodyLimit int    `env:"APP_JSON_BODY_LIMIT" envDefault:"1048576"` // 1 MiB
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`
	// RequestTimeout plus PostHandlerReserve must fit the shutdown drain window
	// (ShutdownBudget - ShutdownReserve) so an in-flight request, including its
	// audit and idempotency bookkeeping, finishes before the process stops
	// waiting for it.
	RequestTimeout           time.Duration `env:"APP_REQUEST_TIMEOUT" envDefault:"12s"`
	FrontendURL              string        `env:"APP_FRONTEND_URL" envDefault:"http://localhost:3000"`
	RequireEmailVerification bool          `env:"REQUIRE_EMAIL_VERIFICATION" envDefault:"false"`
	// TrustProxy enables reading the client IP from ProxyHeader only when the
	// direct peer is in TrustedProxies. Required for correct rate limiting
	// behind a reverse proxy; leaving it off prevents X-Forwarded-For spoofing.
	TrustProxy     bool     `env:"APP_TRUST_PROXY" envDefault:"false"`
	TrustedProxies []string `env:"APP_TRUSTED_PROXIES" envSeparator:","`
	// MetricsAuthToken, when set, requires a matching Bearer token to access
	// the /metrics endpoint. Leave empty to keep /metrics open (local/dev only).
	MetricsAuthToken string `env:"METRICS_AUTH_TOKEN"`
	// ErrorDocsBaseURL is prepended to the RFC 9457 problem "type" URI, e.g.
	// "https://docs.example.com" yields ".../errors/not-found". Empty = relative.
	ErrorDocsBaseURL string `env:"ERROR_DOCS_BASE_URL"`
}

// ShutdownBudget is the total graceful-shutdown time after SIGTERM.
// Kubernetes allows terminationGracePeriodSeconds (30s) including the 5s
// preStop sleep, so 20s leaves a 5s margin before SIGKILL. ShutdownReserve is
// held back from draining requests and jobs for the tracing flush and closing
// shared resources. Keep deploy/k8s/base/deployment.yaml in sync.
const (
	ShutdownBudget  = 20 * time.Second
	ShutdownReserve = 5 * time.Second
)

// PostHandlerReserve is the work a request still does after its handler
// returns: the audit write (2s) plus the idempotency response store and lock
// release (1s). It mirrors app.PostHandlerBudget, which config cannot import.
const PostHandlerReserve = 3 * time.Second

// MaxRequestTimeout is the largest APP_REQUEST_TIMEOUT that still lets an
// in-flight request, with its post-handler work, finish inside the shutdown
// drain window.
const MaxRequestTimeout = ShutdownBudget - ShutdownReserve - PostHandlerReserve

// DefaultJSONBodyLimit is the APP_JSON_BODY_LIMIT default (1 MiB).
const DefaultJSONBodyLimit = 1 << 20

// TrustedProxiesList returns the parsed list of trusted proxy IPs/CIDRs.
func (a AppConfig) TrustedProxiesList() []string {
	return trimmedList(a.TrustedProxies)
}

// EffectiveJSONBodyLimit returns JSONBodyLimit, or the 1 MiB default for a
// zero value. Load rejects explicit values below 1; the fallback only covers
// AppConfig literals built in tests and tools.
func (a AppConfig) EffectiveJSONBodyLimit() int {
	if a.JSONBodyLimit <= 0 {
		return DefaultJSONBodyLimit
	}
	return a.JSONBodyLimit
}

// EffectiveRole returns Role, or RoleAll for a zero value. Load always sets
// the APP_ROLE default; the fallback only covers AppConfig literals built in
// tests and tools.
func (a AppConfig) EffectiveRole() string {
	if a.Role == "" {
		return RoleAll
	}
	return a.Role
}

// ServesAPI reports whether the process mounts the /api routes.
func (a AppConfig) ServesAPI() bool {
	return a.EffectiveRole() != RoleWorker
}

// RunsWorker reports whether the process runs the job worker, the scheduler,
// and module background loops.
func (a AppConfig) RunsWorker() bool {
	return a.EffectiveRole() != RoleAPI
}

// validateAppEnv rejects unknown APP_ENV values. normalized is the value
// compared; raw is echoed in the error so the operator sees what they set.
func validateAppEnv(normalized, raw string) error {
	switch normalized {
	case envLocal, envTest, envDevelopment, envStaging, envProduction:
		return nil
	default:
		return fmt.Errorf("APP_ENV must be one of: local, test, development, staging, production (got %q)", raw)
	}
}

// validate checks the listener, environment, request timeout, and frontend URL.
func (a AppConfig) validate() error {
	if a.Port < 1 || a.Port > maxTCPPort {
		return fmt.Errorf("APP_PORT must be between 1 and %d", maxTCPPort)
	}
	if err := validateAppEnv(a.Env, a.Env); err != nil {
		return err
	}
	switch a.EffectiveRole() {
	case RoleAll, RoleAPI, RoleWorker:
	default:
		return fmt.Errorf("APP_ROLE must be one of: all, api, worker (got %q)", a.Role)
	}
	if a.RequestTimeout < time.Second {
		return fmt.Errorf("APP_REQUEST_TIMEOUT must be at least 1s")
	}
	if a.RequestTimeout > MaxRequestTimeout {
		return fmt.Errorf("APP_REQUEST_TIMEOUT (%s) must be at most %s so in-flight requests finish within the shutdown budget",
			a.RequestTimeout, MaxRequestTimeout)
	}
	if a.FrontendURL != "" && !isAbsoluteHTTPURL(a.FrontendURL) {
		return fmt.Errorf("APP_FRONTEND_URL must be an absolute http(s) URL")
	}
	return nil
}

// validateBodyLimits keeps the JSON body cap inside the multipart body cap.
func (a AppConfig) validateBodyLimits() error {
	if a.BodyLimit < 1 {
		return fmt.Errorf("APP_BODY_LIMIT must be at least 1 byte")
	}
	if a.JSONBodyLimit < 1 {
		return fmt.Errorf("APP_JSON_BODY_LIMIT must be at least 1 byte")
	}
	if a.JSONBodyLimit > a.BodyLimit {
		return fmt.Errorf("APP_JSON_BODY_LIMIT (%d) must not exceed APP_BODY_LIMIT (%d)", a.JSONBodyLimit, a.BodyLimit)
	}
	return nil
}

// validateTrustedProxies requires an explicit, non-wildcard allowlist when
// proxy headers are trusted; otherwise any client could spoof X-Forwarded-For.
func (a AppConfig) validateTrustedProxies() error {
	if !a.TrustProxy {
		return nil
	}
	proxies := a.TrustedProxiesList()
	if len(proxies) == 0 {
		return fmt.Errorf("APP_TRUSTED_PROXIES must list at least one IP/CIDR when APP_TRUST_PROXY=true")
	}
	for _, proxy := range proxies {
		if proxy == "0.0.0.0/0" || proxy == "::/0" {
			return fmt.Errorf("APP_TRUSTED_PROXIES must not trust every address (%s)", proxy)
		}
	}
	return nil
}

// validateProductionBodyLimit leaves room for multipart framing around the
// largest allowed upload.
func (a AppConfig) validateProductionBodyLimit(maxFileSize int64) error {
	if a.BodyLimit <= int(maxFileSize) {
		return fmt.Errorf("APP_BODY_LIMIT (%d) must be greater than STORAGE_MAX_FILE_SIZE (%d) to allow multipart framing", a.BodyLimit, maxFileSize)
	}
	return nil
}

// validateProductionFrontendURL keeps reset/verification links on https.
func (a AppConfig) validateProductionFrontendURL() error {
	if !isHTTPSURL(a.FrontendURL) {
		return fmt.Errorf("APP_FRONTEND_URL must use https in production")
	}
	return nil
}

// validateProductionMetricsToken keeps /metrics behind a bearer token.
func (a AppConfig) validateProductionMetricsToken() error {
	if a.MetricsAuthToken == "" {
		return fmt.Errorf("METRICS_AUTH_TOKEN must be set in production")
	}
	if len(a.MetricsAuthToken) < minMetricsTokenLength {
		return fmt.Errorf("METRICS_AUTH_TOKEN must be at least %d characters", minMetricsTokenLength)
	}
	return nil
}

// isAbsoluteHTTPURL reports whether raw parses as an http(s) URL with a host.
func isAbsoluteHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// isHTTPSURL reports whether raw parses with the https scheme.
func isHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https"
}
