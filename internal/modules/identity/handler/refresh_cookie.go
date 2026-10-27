package handler

import (
	"net/http"
	"slices"
	"strings"
	"time"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const (
	// RefreshCookieName is the BFF refresh-token cookie. The __Host- prefix
	// forces Secure, Path=/ and no Domain (RFC 6265bis), so a subdomain cannot
	// set or overwrite it.
	RefreshCookieName = "__Host-refresh"
	// CSRFHeader is the custom header a browser client sends when the cookie
	// is used and Sec-Fetch-Site is unavailable.
	CSRFHeader = "X-CSRF-Token"
)

// RefreshCookieConfig is the browser token transport (AUTH_REFRESH_COOKIE).
// JSON mode keeps the refresh token in the response body for non-browser
// clients; cookie mode keeps it out of JavaScript and requires a same-origin
// request.
type RefreshCookieConfig struct {
	Enabled bool
	// TTL mirrors JWT_REFRESH_TTL.
	TTL time.Duration
	// Origins are the trusted CORS origins used for the Origin fallback.
	Origins []string
}

// SetRefreshCookie installs the browser token transport. It is called once
// during route registration, before the handler serves requests.
func (h *AuthHandler) SetRefreshCookie(cfg RefreshCookieConfig) { h.refreshCookie = cfg }

// SetRefreshCookie installs the browser token transport used when an MFA
// response issues a token pair (confirm and two-step verify).
func (h *MFAHandler) SetRefreshCookie(cfg RefreshCookieConfig) { h.refreshCookie = cfg }

// SetRefreshCookie installs the browser token transport for passkey login.
func (h *PasskeyHandler) SetRefreshCookie(cfg RefreshCookieConfig) { h.refreshCookie = cfg }

// csrfRefused is the typed 403 for a cookie-authenticated request that is not
// demonstrably same-origin.
func csrfRefused() error {
	return apperror.NewCode(identitycatalog.CodeAuthCSRFFailed,
		"cookie-authenticated requests must be same-origin")
}

// cookieHeader builds the Set-Cookie value for the refresh token.
func (c RefreshCookieConfig) cookieHeader(token string) string {
	if !c.Enabled || token == "" {
		return ""
	}
	maxAge := int(c.TTL / time.Second)
	if maxAge < 0 {
		maxAge = 0
	}
	return (&http.Cookie{
		Name:     RefreshCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}).String()
}

// clearCookieHeader builds the Set-Cookie value that expires the refresh
// cookie on logout.
func (c RefreshCookieConfig) clearCookieHeader() string {
	if !c.Enabled {
		return ""
	}
	return (&http.Cookie{
		Name:     RefreshCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}).String()
}

// jsonRefreshToken keeps the refresh token out of the JSON response in cookie
// mode.
func (c RefreshCookieConfig) jsonRefreshToken(token string) string {
	if c.Enabled {
		return ""
	}
	return token
}

// csrfRequest is the transport subset the CSRF rule needs.
type csrfRequest struct {
	Cookie       string
	SecFetchSite string
	Origin       string
	CSRFToken    string
}

// refreshTokenFromCookie reads the cookie and any CSRF failure. fromCookie is
// true whenever the cookie was present, even if the CSRF check refused it, so
// the caller never falls back to an attacker-supplied body token.
func (c RefreshCookieConfig) refreshTokenFromCookie(req csrfRequest) (token string, fromCookie bool, err error) {
	if !c.Enabled {
		return "", false, nil
	}
	token = cookieValue(req.Cookie, RefreshCookieName)
	if token == "" {
		return "", false, nil
	}
	if csrfErr := c.checkCSRF(req); csrfErr != nil {
		return "", true, csrfErr
	}
	return token, true, nil
}

// checkCSRF enforces the BFF rule: a same-origin/same-site fetch is accepted,
// otherwise a trusted Origin plus the custom header is required. An explicit
// cross-site/none fetch is never accepted.
func (c RefreshCookieConfig) checkCSRF(req csrfRequest) error {
	switch strings.ToLower(strings.TrimSpace(req.SecFetchSite)) {
	case "same-origin", "same-site":
		return nil
	case "cross-site", "none":
		return csrfRefused()
	}
	origin := strings.TrimSpace(req.Origin)
	if origin == "" || !slices.Contains(c.Origins, origin) {
		return csrfRefused()
	}
	if strings.TrimSpace(req.CSRFToken) == "" {
		return csrfRefused()
	}
	return nil
}

// requestRefreshToken returns the cookie token when present (after its CSRF
// check) and otherwise the optional JSON body token.
func (h *AuthHandler) requestRefreshToken(req csrfRequest, body *dto.RefreshRequest) (string, error) {
	token, fromCookie, err := h.refreshCookie.refreshTokenFromCookie(req)
	if err != nil {
		return "", err
	}
	if fromCookie {
		return token, nil
	}
	if body == nil || strings.TrimSpace(body.RefreshToken) == "" {
		return "", apperror.NewBadRequest("refreshToken is required")
	}
	return body.RefreshToken, nil
}

// loginOutput is a token response that may also set the refresh cookie.
type loginOutput struct {
	SetCookie string `header:"Set-Cookie" doc:"Browser BFF refresh cookie in cookie mode."`
	Body      dto.LoginResponse
}

// mfaConfirmOutput carries the recovery codes and re-issued pair, and sets the
// rotated refresh cookie in cookie mode.
type mfaConfirmOutput struct {
	SetCookie string `header:"Set-Cookie" doc:"Browser BFF refresh cookie in cookie mode."`
	Body      dto.MFAConfirmResponse
}

// logoutOutput is a 204 response that expires the refresh cookie in cookie
// mode.
type logoutOutput struct {
	Status    int    `json:"-"`
	SetCookie string `header:"Set-Cookie" doc:"Expires the browser refresh cookie."`
}

// HTTPStatus makes httpapi.Register document the 204 response.
func (*logoutOutput) HTTPStatus() int { return http.StatusNoContent }
