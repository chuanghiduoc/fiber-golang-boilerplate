package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// actorID returns the authenticated subject's user id. A missing subject is a
// programming error on a protected route; protected handlers call it only
// after the authentication middleware ran.
func actorID(ctx context.Context) uuid.UUID {
	if subject, ok := authz.SubjectFromContext(ctx); ok {
		return subject.UserID
	}
	return uuid.Nil
}

// authenticatedWithAPIKey reports whether the request used an API key. It is
// used by destructive self-service operations that a tenant-scoped key must
// not perform.
func authenticatedWithAPIKey(ctx context.Context) bool {
	subject, ok := authz.SubjectFromContext(ctx)
	return ok && subject.Attribute(authz.AttributeAuthMethod) == apikey.AuthMethodAPIKey
}

// withClientMetadata attaches the client IP and User-Agent recorded once by
// the platform middleware to ctx for the services that key login lockout and
// session records on them. Every operation uses it, public or authenticated.
func withClientMetadata(ctx context.Context) context.Context {
	client := requestctx.ClientInfoFromContext(ctx)
	return core.WithUserAgent(core.WithClientIP(ctx, client.IP), client.UserAgent)
}

// sessionIDFromBearer extracts the session id from the already-verified
// access token. Identity's session-bound operations need the id, which the
// authentication middleware keeps in Fiber locals rather than the request
// context. A missing, API-key, or malformed credential yields uuid.Nil.
func sessionIDFromBearer(keyring *token.Keyring, authorization string) uuid.UUID {
	if keyring == nil {
		return uuid.Nil
	}
	parts := strings.SplitN(strings.TrimSpace(authorization), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return uuid.Nil
	}
	claims, err := keyring.Parse(strings.TrimSpace(parts[1]))
	if err != nil {
		return uuid.Nil
	}
	return claims.SessionID
}

// cookieValue returns one cookie from a Cookie request header.
func cookieValue(header, name string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

// etagOpaque converts a last-change timestamp into an opaque strong validator.
func etagOpaque(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UTC().UnixNano(), 10) + "v"
}

// etagHeader quotes the opaque validator for the ETag response header.
func etagHeader(t time.Time) string {
	opaque := etagOpaque(t)
	if opaque == "" {
		return ""
	}
	return `"` + opaque + `"`
}

// checkETag enforces If-Match against the resource's last-change timestamp.
// A missing header leaves the update unconditional.
func checkETag(ifMatch string, updatedAt time.Time) error {
	return httpapi.CheckIfMatchValue(ifMatch, etagOpaque(updatedAt))
}

// decodeMergePatch decodes an RFC 7396 merge-patch document into a typed DTO
// whose optional members use patch.Field. The body is read raw because the
// foundation does not register the application/merge-patch+json media type.
func decodeMergePatch(raw []byte, out any) error {
	if strings.TrimSpace(string(raw)) == "" {
		return apperror.NewBadRequest("request body is required")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		var typeErr *json.UnmarshalTypeError
		var syntaxErr *json.SyntaxError
		if errors.As(err, &typeErr) || errors.As(err, &syntaxErr) {
			return apperror.NewBadRequest("invalid JSON body")
		}
		return apperror.NewBadRequest("invalid merge patch body")
	}
	return nil
}

// parseUUID parses a required path parameter that huma documents as a UUID.
func parseUUID(value, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, apperror.NewBadRequest("invalid " + name)
	}
	return id, nil
}

// unprocessable returns the 422 problem for a semantically invalid merge
// patch.
func unprocessable(detail string) *apperror.AppError {
	return apperror.NewForStatus(http.StatusUnprocessableEntity, detail)
}

// auditActor attributes the request's audit fallback to userID. The request
// info is a pointer shared with the outer audit middleware, so mutating it is
// visible after the handler returns.
func auditActor(ctx context.Context, userID uuid.UUID) {
	if userID == uuid.Nil {
		return
	}
	_ = audit.WithActor(ctx, userID, "")
}

// tenantIDFromContext returns the request tenant, or uuid.Nil.
func tenantIDFromContext(ctx context.Context) uuid.UUID {
	return tenant.IDFromContext(ctx)
}

// paginationLimit clamps a requested page size to the documented range.
func paginationLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// redirectOutput is a 302 response with only the Location header.
type redirectOutput struct {
	Location  string `header:"Location"`
	SetCookie string `header:"Set-Cookie"`
}

// HTTPStatus makes httpapi.Register document the redirect status.
func (*redirectOutput) HTTPStatus() int { return http.StatusFound }

// exportOutput streams an export document with its download headers.
type exportOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	CacheControl       string `header:"Cache-Control"`
	Body               []byte
}

// HTTPStatus documents the export response as 200.
func (*exportOutput) HTTPStatus() int { return http.StatusOK }
