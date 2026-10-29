package middleware

import (
	"bytes"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// BodyLimitOption customizes a BodyLimit middleware.
type BodyLimitOption func(*bodyLimitConfig)

type bodyLimitConfig struct {
	limit       int
	uploadLimit int
	uploadPaths map[string]struct{}
}

// WithUploadLimit allows multipart/form-data requests on the exact paths to
// use a larger ceiling. The route's handler still parses the form with Fiber's
// body limit, so a streaming server reads at most uploadLimit bytes. A
// non-multipart request on an upload path keeps the smaller JSON limit.
func WithUploadLimit(limit int, paths ...string) BodyLimitOption {
	if limit < 1 {
		panic("middleware: BodyLimit upload limit must be >= 1")
	}
	return func(cfg *bodyLimitConfig) {
		if limit < cfg.limit {
			panic("middleware: BodyLimit upload limit must not be smaller than the JSON limit")
		}
		cfg.uploadLimit = limit
		if cfg.uploadPaths == nil {
			cfg.uploadPaths = make(map[string]struct{}, len(paths))
		}
		for _, path := range paths {
			if path == "" {
				panic("middleware: BodyLimit upload path must not be empty")
			}
			cfg.uploadPaths[normalizeUploadPath(path)] = struct{}{}
		}
	}
}

// BodyLimit rejects request bodies larger than the applicable limit with a
// 413 problem response. The default limit caps every body (JSON, form, raw,
// and multipart outside an upload path) before auth, rate limiting, or
// decoding. Multipart uploads on a path registered with WithUploadLimit may
// use the larger upload limit; every other multipart request is capped like
// any other body.
//
// Compressed request bodies are rejected with 415 on every route. Fiber's
// c.Body() transparently decompresses gzip/br/deflate/zstd, so a small
// compressed payload would pass a size check here and then expand far past
// the limit when a handler binds the body. Clients must send identity bodies.
//
// With Fiber Config.StreamRequestBody enabled, a declared Content-Length over
// the limit is rejected without reading the body. Chunked bodies have no
// declared length, so they are read through a limited reader: at most limit+1
// bytes are buffered, and an over-limit body is rejected as soon as the
// (limit+1)th byte arrives. A body at or under the limit is put back for the
// handler, so binding sees the same bytes as before.
func BodyLimit(limit int, options ...BodyLimitOption) fiber.Handler {
	if limit < 1 {
		// Config validation enforces this; reaching it is a programming error.
		panic("middleware: BodyLimit requires limit >= 1")
	}
	cfg := bodyLimitConfig{limit: limit, uploadLimit: limit}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	return func(c fiber.Ctx) error {
		if !isIdentityEncoding(c.Get(fiber.HeaderContentEncoding)) {
			return apperror.NewUnsupportedMediaType("compressed request bodies are not supported; send Content-Encoding: identity")
		}
		upload := cfg.allowsUpload(c)
		effective := cfg.limit
		if upload {
			effective = cfg.uploadLimit
		}
		if c.Request().Header.ContentLength() > effective {
			return apperror.NewPayloadTooLarge("request body too large")
		}
		if upload {
			// The multipart parser in the upload handler enforces
			// APP_BODY_LIMIT even for a chunked body, and parsing lazily keeps
			// the bytes off this middleware's heap.
			return c.Next()
		}
		if c.Request().Header.ContentLength() < 0 {
			if err := bufferChunkedBody(c, effective); err != nil {
				return err
			}
		}
		// A request whose body is already buffered (tests and non-streaming
		// deployments) reports no stream; enforce the limit on what is there.
		if !c.Request().IsBodyStream() && len(c.Request().Body()) > effective {
			return apperror.NewPayloadTooLarge("request body too large")
		}
		return c.Next()
	}
}

// allowsUpload reports whether c is a multipart request on a path registered
// with WithUploadLimit.
func (cfg bodyLimitConfig) allowsUpload(c fiber.Ctx) bool {
	if cfg.uploadLimit <= cfg.limit || !isMultipartForm(c.Get(fiber.HeaderContentType)) {
		return false
	}
	_, ok := cfg.uploadPaths[normalizeUploadPath(c.Path())]
	return ok
}

// bufferChunkedBody reads at most limit+1 bytes of a body without a declared
// length and puts an allowed body back for the handler. A nil stream (a
// buffered test request) is left to the caller's buffered-length check.
func bufferChunkedBody(c fiber.Ctx, limit int) error {
	stream := c.Request().BodyStream()
	if stream == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(stream, int64(limit)+1))
	if err != nil {
		return apperror.NewBadRequest("failed to read request body")
	}
	if len(body) > limit {
		return apperror.NewPayloadTooLarge("request body too large")
	}
	// humafiber reads Request().BodyStream() when StreamRequestBody is on, so
	// the buffered bytes must be put back as a stream. SetBody clears the
	// stream, which makes Huma see an empty body and reject the request.
	c.Request().SetBodyStream(bytes.NewReader(body), len(body))
	return nil
}

// isIdentityEncoding reports whether a Content-Encoding header leaves the body
// uncompressed: absent, empty, or only "identity" codings.
func isIdentityEncoding(contentEncoding string) bool {
	for _, coding := range strings.Split(contentEncoding, ",") {
		coding = strings.TrimSpace(coding)
		if coding != "" && !strings.EqualFold(coding, "identity") {
			return false
		}
	}
	return true
}

func isMultipartForm(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), fiber.MIMEMultipartForm)
}

// normalizeUploadPath matches a request path the way Fiber's default router
// does: case-insensitively and ignoring trailing slashes.
func normalizeUploadPath(path string) string {
	path = strings.ToLower(path)
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}
