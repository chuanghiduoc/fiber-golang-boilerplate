// Package features evaluates compile-time feature flags with optional
// database-backed defaults and tenant/user overrides.
//
// The catalog is code-owned: each module declares its flags in its catalog
// package and registers them at startup through Register (wired by
// app.RegisterCatalogs from app.FlagProvider). Only registered keys can be
// configured through FEATURE_FLAGS, the admin API, or the database, so a typo
// is a startup/API error instead of a silently disabled feature.
package features

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type catalogState struct {
	defs   map[Key]Definition
	owners map[Key]string
}

var (
	registerMu sync.Mutex
	// state is an immutable snapshot published by Register; evaluation reads
	// it without locking.
	state atomic.Pointer[catalogState]
)

func init() {
	state.Store(&catalogState{defs: map[Key]Definition{}, owners: map[Key]string{}})
}

// Register adds owner's flag definitions to the catalog. It fails on an
// invalid key, an empty description, a key listed twice, or a key already
// registered by another owner or with a different definition. Re-registering
// identical definitions for the same owner is a no-op (apps built twice in
// tests). A failed call registers nothing. Register runs at startup, before
// FEATURE_FLAGS is validated against the catalog and before serving requests.
func Register(owner string, defs ...Definition) error {
	if owner == "" {
		return fmt.Errorf("features: register: owner is required")
	}
	registerMu.Lock()
	defer registerMu.Unlock()

	current := state.Load()
	next := &catalogState{
		defs:   make(map[Key]Definition, len(current.defs)+len(defs)),
		owners: make(map[Key]string, len(current.owners)+len(defs)),
	}
	for key, def := range current.defs {
		next.defs[key] = def
		next.owners[key] = current.owners[key]
	}
	seen := make(map[Key]struct{}, len(defs))
	for _, def := range defs {
		if !keyPattern.MatchString(string(def.Key)) {
			return fmt.Errorf("features: %s: invalid flag key %q (want snake_case)", owner, def.Key)
		}
		if strings.TrimSpace(def.Description) == "" {
			return fmt.Errorf("features: %s: flag %q needs a description", owner, def.Key)
		}
		if _, dup := seen[def.Key]; dup {
			return fmt.Errorf("features: %s: flag %q listed twice", owner, def.Key)
		}
		seen[def.Key] = struct{}{}
		if existingOwner, exists := current.owners[def.Key]; exists {
			if existingOwner != owner {
				return fmt.Errorf("features: flag %q registered by %q is already owned by %q", def.Key, owner, existingOwner)
			}
			if current.defs[def.Key] != def {
				return fmt.Errorf("features: %s: flag %q re-registered with a different definition", owner, def.Key)
			}
			continue
		}
		next.defs[def.Key] = def
		next.owners[def.Key] = owner
	}
	state.Store(next)
	return nil
}

// Catalog returns every registered flag definition sorted by key. The
// returned slice is a copy, so callers may mutate it freely.
func Catalog() []Definition {
	defs := state.Load().defs
	out := make([]Definition, 0, len(defs))
	for _, def := range defs {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Lookup resolves a key in the registered catalog.
func Lookup(key string) (Definition, bool) {
	def, ok := state.Load().defs[Key(strings.TrimSpace(key))]
	return def, ok
}

// IsKnown reports whether key is part of the registered catalog.
func IsKnown(key Key) bool {
	_, ok := state.Load().defs[key]
	return ok
}

// OwnerOf returns the owner that registered key and whether key is known.
func OwnerOf(key Key) (string, bool) {
	owner, ok := state.Load().owners[key]
	return owner, ok
}

// CompiledDefault returns the code-defined default for key. Unknown keys are
// disabled so a removed flag cannot turn behavior on by accident.
func CompiledDefault(key Key) bool {
	return state.Load().defs[key].Default
}
