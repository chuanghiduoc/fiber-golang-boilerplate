// Package i18n loads the embedded message catalogs, negotiates a locale from
// Accept-Language, and exposes request-local translation helpers.
//
// Catalogs are flat JSON objects keyed by stable message identifiers. Every
// catalog must carry the same keys and the same {placeholder} names; New
// rejects a bundle that violates parity so a partially translated deployment
// cannot silently fall back to raw keys.
package i18n

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// DefaultLocale is used when the request has no acceptable language and when
// no explicit default is configured.
const DefaultLocale = "en"

// LocaleLocalKey stores the negotiated locale in Fiber locals.
const LocaleLocalKey = "locale"

const messagesDir = "locales"

//go:embed locales/*.json
var catalogsFS embed.FS

type localeCatalog map[string]string

// Bundle is an immutable set of locale catalogs. It is safe for concurrent
// use after New returns.
type Bundle struct {
	defaultLocale string
	catalogs      map[string]localeCatalog
	locales       []string
}

type contextKey struct{}

// WithLocale stores a locale in a context for service-layer consumers.
func WithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, contextKey{}, locale)
}

// LocaleFromContext returns the locale stored by WithLocale, or "".
func LocaleFromContext(ctx context.Context) string {
	if locale, ok := ctx.Value(contextKey{}).(string); ok {
		return locale
	}
	return ""
}

// Locale returns the locale negotiated by Middleware for this request, or "".
func Locale(c fiber.Ctx) string {
	return fiber.Locals[string](c, LocaleLocalKey)
}

// New loads every embedded catalog, validates key/placeholder parity, and
// returns a bundle. An empty defaultLocale means DefaultLocale. An explicit
// unsupported default locale is an error so configuration typos fail at
// startup instead of silently serving English.
func New(defaultLocale string) (*Bundle, error) {
	files, err := fs.Glob(catalogsFS, messagesDir+"/*.json")
	if err != nil {
		return nil, fmt.Errorf("i18n: list embedded catalogs: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("i18n: no embedded catalogs found")
	}

	catalogs := make(map[string]localeCatalog, len(files))
	locales := make([]string, 0, len(files))
	for _, file := range files {
		locale := normalizeLocale(strings.TrimSuffix(path.Base(file), path.Ext(file)))
		if locale == "" {
			return nil, fmt.Errorf("i18n: invalid catalog filename %q", file)
		}
		if _, exists := catalogs[locale]; exists {
			return nil, fmt.Errorf("i18n: duplicate catalog for locale %q", locale)
		}
		raw, err := catalogsFS.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("i18n: read catalog %q: %w", locale, err)
		}
		var messages localeCatalog
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, fmt.Errorf("i18n: decode catalog %q: %w", locale, err)
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("i18n: catalog %q is empty", locale)
		}
		catalogs[locale] = messages
		locales = append(locales, locale)
	}
	sort.Strings(locales)

	if err := validateParity(catalogs, locales); err != nil {
		return nil, err
	}

	if strings.TrimSpace(defaultLocale) == "" {
		defaultLocale = DefaultLocale
	}
	defaultLocale = normalizeLocale(defaultLocale)
	if matched, ok := matchCatalog(catalogs, defaultLocale); ok {
		defaultLocale = matched
	} else {
		return nil, fmt.Errorf("i18n: default locale %q is not supported (have %s)",
			defaultLocale, strings.Join(locales, ", "))
	}

	return &Bundle{
		defaultLocale: defaultLocale,
		catalogs:      catalogs,
		locales:       locales,
	}, nil
}

// DefaultLocale returns the fallback locale configured for the bundle.
func (b *Bundle) DefaultLocale() string {
	if b == nil {
		return DefaultLocale
	}
	return b.defaultLocale
}

// SupportedLocales returns the embedded locale tags in stable order.
func (b *Bundle) SupportedLocales() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.locales))
	copy(out, b.locales)
	return out
}

// Has reports whether key exists in locale, after falling back to the bundle
// default. It is intended for catalog tests and diagnostics.
func (b *Bundle) Has(locale, key string) bool {
	if b == nil {
		return false
	}
	_, ok := b.lookup(locale, key)
	if ok {
		return true
	}
	_, ok = b.catalogs[b.defaultLocale][key]
	return ok
}

// Translate returns the message for key in locale. Locale tags are matched
// case-insensitively and truncated (vi-VN -> vi) against the supported set.
// Missing messages fall back to the bundle default and then to the key itself.
// Parameters replace {name} placeholders; unknown placeholders are preserved.
func (b *Bundle) Translate(locale, key string, params map[string]any) string {
	if b == nil {
		return key
	}
	if message, ok := b.lookup(locale, key); ok {
		return interpolate(message, params)
	}
	if message, ok := b.catalogs[b.defaultLocale][key]; ok {
		return interpolate(message, params)
	}
	return key
}

// Localize implements apperror.Localizer. It resolves the request locale from
// Fiber locals and returns "" when the key is absent so callers can keep their
// existing English text as a fallback.
func (b *Bundle) Localize(c fiber.Ctx, key string, params map[string]any) string {
	if b == nil || key == "" {
		return ""
	}
	if message, ok := b.lookup(Locale(c), key); ok {
		return interpolate(message, params)
	}
	if message, ok := b.catalogs[b.defaultLocale][key]; ok {
		return interpolate(message, params)
	}
	return ""
}

// Negotiate chooses the best supported locale from an Accept-Language header.
// It understands q-values, wildcards, case-insensitive tags, and truncation
// (vi-VN -> vi), and falls back to the bundle default when nothing matches.
func (b *Bundle) Negotiate(acceptLanguage string) string {
	if b == nil {
		return DefaultLocale
	}
	for _, pref := range parseAcceptLanguage(acceptLanguage) {
		if locale, ok := b.match(pref.tag); ok {
			return locale
		}
	}
	return b.defaultLocale
}

// Middleware negotiates the request locale, stores it in Fiber locals and the
// request context, and sets Content-Language and Vary: Accept-Language.
func (b *Bundle) Middleware() fiber.Handler {
	return Middleware(b)
}

// Middleware returns a Fiber handler for bundle. A nil bundle is a pass-through
// so tests and minimal deployments can omit i18n entirely.
func Middleware(b *Bundle) fiber.Handler {
	if b == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}
	return func(c fiber.Ctx) error {
		locale := b.Negotiate(c.Get(fiber.HeaderAcceptLanguage))
		fiber.Locals[string](c, LocaleLocalKey, locale)
		c.SetContext(WithLocale(c.Context(), locale))
		c.Set(fiber.HeaderContentLanguage, locale)
		c.Vary(fiber.HeaderAcceptLanguage)
		return c.Next()
	}
}

func (b *Bundle) lookup(locale, key string) (string, bool) {
	if b == nil || key == "" {
		return "", false
	}
	if matched, ok := b.match(locale); ok {
		if message, exists := b.catalogs[matched][key]; exists {
			return message, true
		}
	}
	if message, exists := b.catalogs[b.defaultLocale][key]; exists {
		return message, true
	}
	return "", false
}

func (b *Bundle) match(locale string) (string, bool) {
	locale = normalizeLocale(locale)
	if locale == "*" {
		return b.defaultLocale, true
	}
	return matchCatalog(b.catalogs, locale)
}

func matchCatalog(catalogs map[string]localeCatalog, locale string) (string, bool) {
	locale = normalizeLocale(locale)
	if locale == "" || locale == "*" {
		return "", false
	}
	if _, ok := catalogs[locale]; ok {
		return locale, true
	}
	for {
		idx := strings.LastIndexByte(locale, '-')
		if idx < 0 {
			return "", false
		}
		locale = locale[:idx]
		if _, ok := catalogs[locale]; ok {
			return locale, true
		}
	}
}

type languagePref struct {
	tag string
	q   float64
}

// parseAcceptLanguage parses language ranges and orders them by descending
// q-value. Equal q-values keep their header order. Invalid q-values are treated
// as unacceptable (q=0), matching the RFC's range [0,1].
func parseAcceptLanguage(header string) []languagePref {
	parts := strings.Split(header, ",")
	prefs := make([]languagePref, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, ";")
		tag := normalizeLocale(fields[0])
		if tag == "" {
			continue
		}
		q := 1.0
		for _, param := range fields[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || math.IsNaN(parsed) || parsed < 0 || parsed > 1 {
				q = 0
				break
			}
			q = parsed
		}
		if q == 0 {
			continue
		}
		prefs = append(prefs, languagePref{tag: tag, q: q})
	}
	sort.SliceStable(prefs, func(i, j int) bool { return prefs[i].q > prefs[j].q })
	return prefs
}

func normalizeLocale(locale string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(locale), "_", "-"))
}

func interpolate(message string, params map[string]any) string {
	if len(params) == 0 || !strings.Contains(message, "{") {
		return message
	}
	var out strings.Builder
	out.Grow(len(message))
	for i := 0; i < len(message); {
		start := strings.IndexByte(message[i:], '{')
		if start < 0 {
			out.WriteString(message[i:])
			break
		}
		start += i
		out.WriteString(message[i:start])
		end := strings.IndexByte(message[start:], '}')
		if end < 0 {
			out.WriteString(message[start:])
			break
		}
		end += start
		key := strings.TrimSpace(message[start+1 : end])
		if value, ok := params[key]; ok {
			fmt.Fprint(&out, value)
		} else {
			out.WriteString(message[start : end+1])
		}
		i = end + 1
	}
	return out.String()
}

func validateParity(catalogs map[string]localeCatalog, locales []string) error {
	reference := locales[0]
	referenceMessages := catalogs[reference]
	for _, locale := range locales[1:] {
		messages := catalogs[locale]
		missing, extra := diffKeys(referenceMessages, messages)
		if len(missing) > 0 || len(extra) > 0 {
			return fmt.Errorf("i18n: catalog %q does not match %q (missing keys: %s; extra keys: %s)",
				locale, reference, strings.Join(missing, ", "), strings.Join(extra, ", "))
		}
		for key, referenceMessage := range referenceMessages {
			want := placeholders(referenceMessage)
			got := placeholders(messages[key])
			if !equalStrings(want, got) {
				return fmt.Errorf("i18n: catalog %q key %q placeholders %v do not match %q placeholders %v",
					locale, key, got, reference, want)
			}
		}
	}
	return nil
}

func diffKeys(reference, candidate localeCatalog) (missing, extra []string) {
	for key := range reference {
		if _, ok := candidate[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range candidate {
		if _, ok := reference[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func placeholders(message string) []string {
	seen := make(map[string]struct{})
	for i := 0; i < len(message); {
		start := strings.IndexByte(message[i:], '{')
		if start < 0 {
			break
		}
		start += i
		end := strings.IndexByte(message[start:], '}')
		if end < 0 {
			break
		}
		end += start
		name := strings.TrimSpace(message[start+1 : end])
		if name != "" {
			seen[name] = struct{}{}
		}
		i = end + 1
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
