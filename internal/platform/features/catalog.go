// Package features evaluates compile-time feature flags with optional
// database-backed defaults and tenant/user overrides.
//
// The catalog is code-owned but injected: each module declares its flags in
// its catalog package and the composition root builds one features.Catalog
// from the app.FlagProviders, detects duplicate/conflicting keys, seals it, and
// injects it into the evaluator, the admin service, FEATURE_FLAGS validation,
// and route middleware. Only keys in that catalog can be configured through
// FEATURE_FLAGS, the admin API, or the database, so a typo is a startup/API
// error instead of a silently disabled feature. There is deliberately no
// process-wide mutable registry.
package features

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Key identifies a feature flag. Values are stable snake_case identifiers.
type Key string

// Definition describes one compile-time flag.
type Definition struct {
	Key         Key
	Description string
	// Default is used when no higher-precedence source (force-off, kill
	// switch, target, database default, or environment value) applies.
	Default bool
}

// Provider contributes one owner's flag definitions to a Catalog. The
// composition root builds one provider per app.FlagProvider.
type Provider struct {
	Owner       string
	Definitions []Definition
}

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Catalog is an injected, instance-scoped feature-flag catalog. It is
// populated at the composition root (or by a test helper) and sealed before
// serving, so evaluation reads it under a cheap read lock.
type Catalog struct {
	mu     sync.RWMutex
	defs   map[Key]Definition
	owners map[Key]string
	sealed bool
}

// NewCatalog returns an empty, unsealed flag catalog.
func NewCatalog() *Catalog {
	return &Catalog{
		defs:   map[Key]Definition{},
		owners: map[Key]string{},
	}
}

// EmptyCatalog returns a sealed catalog with no definitions. It is the
// fail-closed default for evaluators built without an injected catalog.
func EmptyCatalog() *Catalog {
	emptyOnce.Do(func() {
		c := NewCatalog()
		c.Seal()
		emptyCatalog = c
	})
	return emptyCatalog
}

var (
	emptyOnce    sync.Once
	emptyCatalog *Catalog
)

// Add contributes owner's flag definitions. It fails on an invalid key, an
// empty description, a key listed twice, a key already registered by another
// owner or with a different definition, and on a sealed catalog.
// Re-adding identical definitions for the same owner is a no-op.
func (c *Catalog) Add(provider Provider) error {
	if provider.Owner == "" {
		return fmt.Errorf("features: add: owner is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed {
		return fmt.Errorf("features: %s: catalog is sealed", provider.Owner)
	}

	pending := make([]Definition, 0, len(provider.Definitions))
	seen := make(map[Key]struct{}, len(provider.Definitions))
	for _, def := range provider.Definitions {
		if !keyPattern.MatchString(string(def.Key)) {
			return fmt.Errorf("features: %s: invalid flag key %q (want snake_case)", provider.Owner, def.Key)
		}
		if strings.TrimSpace(def.Description) == "" {
			return fmt.Errorf("features: %s: flag %q needs a description", provider.Owner, def.Key)
		}
		if _, dup := seen[def.Key]; dup {
			return fmt.Errorf("features: %s: flag %q listed twice", provider.Owner, def.Key)
		}
		seen[def.Key] = struct{}{}
		if existingOwner, exists := c.owners[def.Key]; exists {
			if existingOwner != provider.Owner {
				return fmt.Errorf("features: flag %q registered by %q is already owned by %q", def.Key, provider.Owner, existingOwner)
			}
			if c.defs[def.Key] != def {
				return fmt.Errorf("features: %s: flag %q re-registered with a different definition", provider.Owner, def.Key)
			}
			continue
		}
		pending = append(pending, def)
	}
	for _, def := range pending {
		c.defs[def.Key] = def
		c.owners[def.Key] = provider.Owner
	}
	return nil
}

// Seal prevents further additions.
func (c *Catalog) Seal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sealed = true
}

// Definitions returns every registered flag definition sorted by key. The
// returned slice is a copy, so callers may mutate it freely.
func (c *Catalog) Definitions() []Definition {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Definition, 0, len(c.defs))
	for _, def := range c.defs {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Lookup resolves a key in the catalog.
func (c *Catalog) Lookup(key string) (Definition, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	def, ok := c.defs[Key(strings.TrimSpace(key))]
	return def, ok
}

// IsKnown reports whether key is part of the catalog.
func (c *Catalog) IsKnown(key Key) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.defs[key]
	return ok
}

// OwnerOf returns the owner that registered key and whether key is known.
func (c *Catalog) OwnerOf(key Key) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	owner, ok := c.owners[key]
	return owner, ok
}

// CompiledDefault returns the code-defined default for key. Unknown keys are
// disabled so a removed flag cannot turn behavior on by accident.
func (c *Catalog) CompiledDefault(key Key) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.defs[key].Default
}

// ValidateFlags rejects keys in parsed that are not in this catalog.
func (c *Catalog) ValidateFlags(parsed map[Key]bool) error {
	for key := range parsed {
		if !c.IsKnown(key) {
			return fmt.Errorf("unknown feature flag %q", key)
		}
	}
	return nil
}

// ValidateForceOff rejects force-off keys that are not in this catalog.
func (c *Catalog) ValidateForceOff(forceOff map[Key]struct{}) error {
	for key := range forceOff {
		if !c.IsKnown(key) {
			return fmt.Errorf("unknown force-off feature flag %q", key)
		}
	}
	return nil
}
