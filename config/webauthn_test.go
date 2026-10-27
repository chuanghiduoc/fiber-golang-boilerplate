package config

import (
	"strings"
	"testing"
)

func TestWebAuthnConfig_DisabledWhenEmpty(t *testing.T) {
	cfg := WebAuthnConfig{}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	if cfg.Enabled() {
		t.Fatal("empty section must disable passkeys")
	}
}

func TestWebAuthnConfig_RejectsPartialConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  WebAuthnConfig
		want string
	}{
		{
			name: "origins without rp id",
			cfg:  WebAuthnConfig{RPOrigins: []string{"https://example.com"}},
			want: "WEBAUTHN_RP_ORIGINS requires WEBAUTHN_RP_ID",
		},
		{
			name: "name without rp id",
			cfg:  WebAuthnConfig{RPName: "Example"},
			want: "WEBAUTHN_RP_NAME requires WEBAUTHN_RP_ID",
		},
		{
			name: "rp id without origins",
			cfg:  WebAuthnConfig{RPID: "example.com"},
			want: "WEBAUTHN_RP_ORIGINS is required",
		},
		{
			name: "rp id with scheme",
			cfg:  WebAuthnConfig{RPID: "https://example.com", RPOrigins: []string{"https://example.com"}},
			want: "lower-case domain name",
		},
		{
			name: "rp id with port",
			cfg:  WebAuthnConfig{RPID: "example.com:8443", RPOrigins: []string{"https://example.com:8443"}},
			want: "lower-case domain name",
		},
		{
			name: "ip rp id",
			cfg:  WebAuthnConfig{RPID: "127.0.0.1", RPOrigins: []string{"http://127.0.0.1:3000"}},
			want: "lower-case domain name",
		},
		{
			name: "origin with path",
			cfg:  WebAuthnConfig{RPID: "example.com", RPOrigins: []string{"https://example.com/app"}},
			want: "path is not allowed",
		},
		{
			name: "origin on another domain",
			cfg:  WebAuthnConfig{RPID: "example.com", RPOrigins: []string{"https://evil.test"}},
			want: "not the relying-party domain",
		},
		{
			name: "duplicate origin",
			cfg: WebAuthnConfig{
				RPID:      "example.com",
				RPOrigins: []string{"https://example.com", "https://example.com"},
			},
			want: "duplicate origin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestWebAuthnConfig_AcceptsSubdomainOrigin(t *testing.T) {
	cfg := WebAuthnConfig{
		RPID:      "example.com",
		RPName:    "Example",
		RPOrigins: []string{"https://app.example.com", "https://example.com:8443"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	if got := cfg.DisplayName(); got != "Example" {
		t.Fatalf("DisplayName() = %q", got)
	}
	if len(cfg.OriginList()) != 2 {
		t.Fatalf("OriginList() = %v", cfg.OriginList())
	}
}

func TestWebAuthnConfig_LocalhostIsAllowedInDevelopment(t *testing.T) {
	cfg := WebAuthnConfig{RPID: "localhost", RPOrigins: []string{"http://localhost:3000"}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

func TestWebAuthnConfig_ProductionRequiresSettingsAndHTTPS(t *testing.T) {
	if err := (WebAuthnConfig{}).validateProduction(); err == nil {
		t.Fatal("production must require a passkey configuration")
	}
	if err := (WebAuthnConfig{RPID: "example.com", RPName: "Example"}).validateProduction(); err == nil {
		t.Fatal("production must require origins")
	}
	httpCfg := WebAuthnConfig{RPID: "example.com", RPName: "Example", RPOrigins: []string{"http://example.com"}}
	if err := httpCfg.validateProduction(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("validateProduction() = %v, want https error", err)
	}
	good := WebAuthnConfig{RPID: "example.com", RPName: "Example", RPOrigins: []string{"https://example.com"}}
	if err := good.validateProduction(); err != nil {
		t.Fatalf("validateProduction() = %v, want nil", err)
	}
}
