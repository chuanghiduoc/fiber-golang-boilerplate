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

func (g *GoogleOAuth) AuthURL(state string) string {
	return g.cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)
}

// BuildCallbackURL constructs the redirect URL with tokens in the URL fragment.
// Fragment data is never sent to the server, preventing token leakage via Referer headers.
func (g *GoogleOAuth) BuildCallbackURL(accessToken, refreshToken string) string {
	params := url.Values{}
	params.Set("access_token", accessToken)
	params.Set("refresh_token", refreshToken)

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

func (g *GoogleOAuth) Exchange(ctx context.Context, code string) (*GoogleUserInfo, error) {
	token, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}

	client := g.cfg.Client(ctx, token)
	resp, err := client.Get(googleUserInfoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("google userinfo returned status %d: %s", resp.StatusCode, body)
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
