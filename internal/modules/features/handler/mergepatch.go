package handler

import (
	"bytes"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humafiber"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

// mergePatchMediaType is the RFC 7396 media type for partial updates.
const mergePatchMediaType = "application/merge-patch+json"

// prepareRequestBody bridges two gaps in the shared huma request path:
//
//   - httpapi.DefaultConfig registers only application/json and huma copies
//     its format table when the API is built, so a module cannot add the RFC
//     7396 media type. Rewriting the request media type before huma decodes
//     the body is the smallest local equivalent: the body is ordinary JSON.
//   - huma's Fiber adapter reads the live body stream, but a Fiber middleware
//     that reads the body first (the idempotency body hash, BodyLimit's
//     chunked-body pass) closes that stream and leaves the bytes buffered.
//     Re-exposing the buffered bytes keeps huma's decoder working on
//     Idempotency-Key requests with a JSON body.
//
// Both gaps are recorded in REPORT-w6b-nfa.md for the foundation owner.
func prepareRequestBody(api *httpapi.API) {
	api.Huma().UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		if requestMediaType(ctx.Header("Content-Type")) == mergePatchMediaType {
			humafiber.Unwrap(ctx).Request().Header.SetContentType("application/json")
		}
		request := humafiber.Unwrap(ctx).Request()
		if !request.IsBodyStream() {
			if body := request.Body(); len(body) > 0 {
				request.SetBodyStream(bytes.NewReader(body), len(body))
			}
		}
		next(ctx)
	})
}

// requestMediaType strips media-type parameters and normalizes case.
func requestMediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}
