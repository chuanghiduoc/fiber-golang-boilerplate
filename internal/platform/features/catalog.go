// Package features evaluates compile-time feature flags with optional
// database-backed defaults and tenant/user overrides.
//
// The catalog is intentionally code-owned: only keys declared here can be
// configured through FEATURE_FLAGS, the admin API, or the database. A typo is
// a startup/API error instead of a silently disabled feature.
package features

import (
	"sort"
	"strings"
)

// Key identifies a feature flag. Values are stable snake_case identifiers.
type Key string

// Compile-time flag catalog. Add a key here before referencing it anywhere
// else; the admin API and environment parser reject unknown keys.
const (
	KeyUserRegistration  Key = "user_registration"
	KeyEmailVerification Key = "email_verification"
	KeyGoogleOAuth       Key = "google_oauth"
	KeyFileUploads       Key = "file_uploads"
)

// Definition describes one compile-time flag.
type Definition struct {
	Key         Key
	Description string
	// Default is used when no higher-precedence source (force-off, kill
	// switch, target, database default, or environment value) applies.
	Default bool
}

var catalog = []Definition{
	{
		Key:         KeyUserRegistration,
		Description: "Allow new users to register through the public API.",
		Default:     true,
	},
	{
		Key:         KeyEmailVerification,
		Description: "Require email verification before authenticated actions.",
		Default:     false,
	},
	{
		Key:         KeyGoogleOAuth,
		Description: "Expose Google OAuth login and callback routes.",
		Default:     false,
	},
	{
		Key:         KeyFileUploads,
		Description: "Allow authenticated file uploads.",
		Default:     true,
	},
}

var catalogByKey = func() map[Key]Definition {
	out := make(map[Key]Definition, len(catalog))
	for _, def := range catalog {
		out[def.Key] = def
	}
	return out
}()

// Catalog returns every compile-time flag definition sorted by key. The
// returned slice is a copy, so callers may mutate it freely.
func Catalog() []Definition {
	out := make([]Definition, len(catalog))
	copy(out, catalog)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Lookup resolves a key in the compile-time catalog.
func Lookup(key string) (Definition, bool) {
	def, ok := catalogByKey[Key(strings.TrimSpace(key))]
	return def, ok
}

// IsKnown reports whether key is part of the compile-time catalog.
func IsKnown(key Key) bool {
	_, ok := catalogByKey[key]
	return ok
}

// CompiledDefault returns the code-defined default for key. Unknown keys are
// disabled so a removed flag cannot turn behavior on by accident.
func CompiledDefault(key Key) bool {
	def, ok := catalogByKey[key]
	if !ok {
		return false
	}
	return def.Default
}
