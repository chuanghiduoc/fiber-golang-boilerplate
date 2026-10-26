package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

const googleUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

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
	token, err := g.cfg.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}

	client := g.cfg.Client(ctx, token)
	resp, err := client.Get(g.userInfoURL)
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

	var info GoogleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	if info.ID == "" || info.Email == "" {
		return nil, fmt.Errorf("incomplete user info from Google")
	}
	if !info.VerifiedEmail {
		return nil, fmt.Errorf("google email address is not verified")
	}

	return &info, nil
}

func (g *GoogleOAuth) FrontendURL() string {
	return g.frontendURL
}
