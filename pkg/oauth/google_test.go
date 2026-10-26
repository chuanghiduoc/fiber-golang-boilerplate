package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/oauth2"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func configForTest() config.OAuthConfig {
	return config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "https://api.example.test/api/v1/auth/google/callback",
		FrontendURL:        "https://app.example.test/auth/callback",
	}
}

func TestAuthURLUsesPKCES256AndNoOfflineAccess(t *testing.T) {
	g := NewGoogleOAuth(configForTest())
	verifier := oauth2.GenerateVerifier()

	raw := g.AuthURL("signed-state", verifier)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		t.Fatal("code_challenge is empty")
	}
	if challenge == verifier {
		t.Error("code_challenge must be the SHA-256 challenge, not the verifier")
	}
	if got := q.Get("state"); got != "signed-state" {
		t.Errorf("state = %q", got)
	}
	if got := q.Get("access_type"); got != "" {
		t.Errorf("access_type = %q, want unset (no refresh token is used)", got)
	}
}

func TestBuildCodeCallbackURL(t *testing.T) {
	g := NewGoogleOAuth(configForTest())
	raw := g.BuildCodeCallbackURL("one-time-code")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse callback url: %v", err)
	}
	if u.Fragment != "code=one-time-code" {
		t.Errorf("fragment = %q, want the one-time code only", u.Fragment)
	}
	if u.RawQuery != "" {
		t.Errorf("query = %q, want empty", u.RawQuery)
	}
}

func TestExchangeSendsCodeVerifier(t *testing.T) {
	var gotVerifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			gotVerifier = r.PostForm.Get("code_verifier")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "provider-access", "token_type": "Bearer", "expires_in": 3600,
			})
		case "/userinfo":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "google-1", "email": "user@example.com", "verified_email": true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := configForTest()
	g := &GoogleOAuth{
		cfg: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.GoogleRedirectURL,
			Scopes:       []string{"email", "profile"},
			Endpoint:     oauth2.Endpoint{AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token"},
		},
		frontendURL: cfg.FrontendURL,
		userInfoURL: srv.URL + "/userinfo",
	}

	info, err := g.Exchange(context.Background(), "auth-code", "the-verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotVerifier != "the-verifier" {
		t.Errorf("code_verifier = %q, want the verifier", gotVerifier)
	}
	if info.Email != "user@example.com" || !info.VerifiedEmail {
		t.Errorf("user info = %+v", info)
	}
}
