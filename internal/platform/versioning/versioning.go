// Package versioning implements API version selection and lifecycle metadata.
//
// The path remains the source of truth: /api/v1 and /api/v2 are mounted as
// separate groups. Accept-Version is an optional assertion: when present it
// must match the path version, otherwise the request fails with 406
// api.version_unsupported. This keeps clients on path routing while giving
// them a way to detect accidental version drift.
package versioning

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Header is the optional client version assertion.
const Header = "Accept-Version"

// ResponseHeader echoes the version that served the request.
const ResponseHeader = "API-Version"

// Version identifies a public API surface.
type Version string

const (
	// V1 is the baseline, stable API surface.
	V1 Version = "v1"
	// V2 is the opt-in successor surface.
	V2 Version = "v2"
)

// Path returns the mount path for the version.
func (v Version) Path() string {
	return "/api/" + string(v)
}

// Valid reports whether v is a known public version.
func (v Version) Valid() bool {
	return v == V1 || v == V2
}

// ParseAcceptVersion accepts the compact and prefixed forms 1, v1, 2, and v2.
// Matching is case-insensitive and ignores surrounding whitespace. An empty
// value is not a valid assertion; callers treat an absent header separately.
func ParseAcceptVersion(value string) (Version, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "v1":
		return V1, true
	case "2", "v2":
		return V2, true
	default:
		return "", false
	}
}

// Policy carries lifecycle metadata for one version. Zero timestamps mean the
// version is not deprecated and has no sunset date.
type Policy struct {
	DeprecatedAt time.Time
	SunsetAt     time.Time
	Successor    Version
	DocsURL      string
}

// Config describes the version lifecycle. The zero value and a nil *Config
// both mean v1, current, with no deprecation metadata.
type Config struct {
	Default  Version
	Policies map[Version]Policy
}

// DefaultVersion returns the configured default, or V1 for nil/zero configs.
func (c *Config) DefaultVersion() Version {
	if c == nil || !c.Default.Valid() {
		return V1
	}
	return c.Default
}

// PolicyFor returns the policy for version. Missing policies are zero-valued
// (not deprecated), keeping v2 opt-in surfaces free of unnecessary headers.
func (c *Config) PolicyFor(version Version) Policy {
	if c == nil || c.Policies == nil {
		return Policy{}
	}
	return c.Policies[version]
}

// Middleware asserts Accept-Version (when supplied), echoes API-Version, and
// emits deprecation metadata once DeprecatedAt has passed. It always adds
// Vary: Accept-Version so shared caches keep versions separate.
func Middleware(version Version, cfg *Config) fiber.Handler {
	policy := cfg.PolicyFor(version)
	return func(c fiber.Ctx) error {
		c.Set(ResponseHeader, string(version))
		c.Vary(Header)

		if raw := strings.TrimSpace(c.Get(Header)); raw != "" {
			requested, ok := ParseAcceptVersion(raw)
			if !ok || requested != version {
				return apperror.NewCode(
					apperror.CodeAPIVersionUnsupported,
					fmt.Sprintf("Accept-Version %q is not supported; this endpoint serves %s", raw, version),
				).WithDetailKey("errors.api.version_unsupported.detail", map[string]any{"version": raw})
			}
		}

		applyDeprecation(c, policy, time.Now())
		return c.Next()
	}
}

// Guard rejects version-shaped paths that were not mounted, for example
// /api/v3. Non-version paths pass through so ordinary 404 handling is
// unchanged.
func Guard(versions ...Version) fiber.Handler {
	supported := make(map[Version]struct{}, len(versions))
	for _, version := range versions {
		supported[version] = struct{}{}
	}
	return func(c fiber.Ctx) error {
		c.Vary(Header)
		version, ok := versionFromPath(c.Path())
		if !ok {
			return c.Next()
		}
		if _, ok := supported[version]; !ok {
			return apperror.NewCode(
				apperror.CodeAPIVersionUnsupported,
				fmt.Sprintf("API version %q is not supported", version),
			).WithDetailKey("errors.api.version_unsupported.detail", map[string]any{"version": string(version)})
		}
		return c.Next()
	}
}

func applyDeprecation(c fiber.Ctx, policy Policy, now time.Time) {
	if policy.DeprecatedAt.IsZero() || now.Before(policy.DeprecatedAt) {
		return
	}
	c.Set("Deprecation", "@"+strconv.FormatInt(policy.DeprecatedAt.Unix(), 10))
	if !policy.SunsetAt.IsZero() {
		c.Set("Sunset", policy.SunsetAt.UTC().Format(http.TimeFormat))
	}

	links := make([]string, 0, 2)
	if policy.DocsURL != "" {
		links = append(links, fmt.Sprintf("<%s>; rel=\"deprecation\"", policy.DocsURL))
	}
	if policy.Successor.Valid() {
		links = append(links, fmt.Sprintf("<%s>; rel=\"successor-version\"", policy.Successor.Path()))
	}
	if len(links) > 0 {
		c.Set("Link", strings.Join(links, ", "))
	}
}

// versionFromPath extracts a leading v<digits> segment from /api/<segment>.
func versionFromPath(requestPath string) (Version, bool) {
	const prefix = "/api/"
	if !strings.HasPrefix(requestPath, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(requestPath, prefix)
	segment, _, _ := strings.Cut(remainder, "/")
	if len(segment) < 2 || segment[0] != 'v' {
		return "", false
	}
	for _, char := range segment[1:] {
		if char < '0' || char > '9' {
			return "", false
		}
	}
	return Version(segment), true
}
