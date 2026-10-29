package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

const (
	googleUserInfoURL      = "https://www.googleapis.com/oauth2/v2/userinfo"
	maxGoogleUserInfoBytes = 64 << 10
	googleHTTPTimeout      = 10 * time.Second
)

// Errors that describe a usable token whose user info cannot be trusted. They
// are sentinels so the HTTP adapter can answer with a client-facing problem
// (the caller can fix an unverified address in their Google account) instead
// of the 502 reserved for a provider outage.
var (
	// ErrEmailNotVerified means Google returned an address the account owner
	// has not verified; linking or creating an account from it would let
	// someone claim an address they do not control.
	ErrEmailNotVerified = errors.New("google account email address is not verified")
	// ErrIncompleteUserInfo means the userinfo response lacked the stable
	// subject id or the email.
	ErrIncompleteUserInfo = errors.New("incomplete user info from Google")
)

type GoogleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	VerifiedEmail bool   `json:"verified_email"`
}

type GoogleOAuth struct {
	cfg         *oauth2.Config
	frontendURL string
	// userInfoURL is Google's userinfo endpoint; a field so tests can point
	// it at a local server.
	userInfoURL string
	// httpTimeout bounds the token exchange plus the userinfo request even when
	// the caller's context has no deadline. Zero uses googleHTTPTimeout.
	httpTimeout time.Duration
}

func NewGoogleOAuth(cfg config.OAuthConfig) *GoogleOAuth {
	return &GoogleOAuth{
		cfg: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.GoogleRedirectURL,
			Scopes:       []string{"email", "profile"},
			Endpoint:     google.Endpoint,
		},
		frontendURL: cfg.FrontendURL,
		userInfoURL: googleUserInfoURL,
		httpTimeout: googleHTTPTimeout,
	}
}

// ValidateFrontendURL checks that the configured frontend URL is parseable and uses http(s).
func (g *GoogleOAuth) ValidateFrontendURL() error {
	parsed, err := url.Parse(g.frontendURL)
	if err != nil {
		return fmt.Errorf("invalid OAUTH_FRONTEND_URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("OAUTH_FRONTEND_URL must use http or https scheme (got %q)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("OAUTH_FRONTEND_URL must have a host")
	}
	return nil
}

// AuthURL builds the Google authorization request with PKCE (S256, RFC 9700)
// and without access_type=offline: the application never uses Google refresh
// tokens, and an offline grant would only widen the consent surface.
func (g *GoogleOAuth) AuthURL(state, codeVerifier string) string {
	return g.cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier))
}

// BuildCodeCallbackURL constructs the redirect to the frontend with a
// single-use code in the URL fragment. Fragment data is never sent to the
// server, so the code does not reach referrer headers or access logs; the
// caller redeems it at POST /auth/google/exchange.
func (g *GoogleOAuth) BuildCodeCallbackURL(code string) string {
	params := url.Values{}
	params.Set("code", code)

	u, err := url.Parse(g.frontendURL)
	if err != nil {
		return g.frontendURL
	}
	u.Fragment = params.Encode()
	return u.String()
}

// BuildMFACallbackURL constructs the redirect URL for a first-factor Google
// sign-in that still needs a TOTP or recovery code. Only the challenge travels
// in the URL fragment, never a token.
func (g *GoogleOAuth) BuildMFACallbackURL(challenge string) string {
	params := url.Values{}
	params.Set("mfa_required", "true")
	params.Set("mfa_challenge", challenge)

	u, err := url.Parse(g.frontendURL)
	if err != nil {
		return g.frontendURL
	}
	u.Fragment = params.Encode()
	return u.String()
}

// UpstreamError is a non-2xx response from a Google endpoint other than the
// token exchange (which oauth2 reports as *oauth2.RetrieveError). It carries
// only the operation and status, never a response body that might echo a
// credential.
type UpstreamError struct {
	Op         string
	StatusCode int
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("google %s returned status %d", e.Op, e.StatusCode)
}

func (g *GoogleOAuth) Exchange(ctx context.Context, code, codeVerifier string) (*GoogleUserInfo, error) {
	timeout := g.httpTimeout
	if timeout <= 0 {
		timeout = googleHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	token, err := g.cfg.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}

	client := g.cfg.Client(ctx, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userInfoURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build user info request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the connection can be reused; the body is
		// deliberately not part of the error or the logs.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, &UpstreamError{Op: "userinfo", StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGoogleUserInfoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read user info: %w", err)
	}
	if len(body) > maxGoogleUserInfoBytes {
		return nil, fmt.Errorf("google userinfo response exceeds %d bytes", maxGoogleUserInfoBytes)
	}

	var info GoogleUserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	if info.ID == "" || info.Email == "" {
		return nil, ErrIncompleteUserInfo
	}
	if !info.VerifiedEmail {
		return nil, ErrEmailNotVerified
	}

	return &info, nil
}

func (g *GoogleOAuth) FrontendURL() string {
	return g.frontendURL
}
