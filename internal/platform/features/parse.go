package features

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseFlags parses the FEATURE_FLAGS environment value. The format is a
// comma-separated list of key=true|false assignments; a bare key is shorthand
// for true, for example "user_registration,email_verification=false". Empty
// input yields an empty map; empty entries and duplicate keys are rejected so
// a typo does not silently change precedence.
func ParseFlags(raw string) (map[Key]bool, error) {
	out := make(map[Key]bool)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		keyPart, valuePart, hasValue := strings.Cut(entry, "=")
		key := Key(strings.TrimSpace(keyPart))
		if key == "" {
			return nil, fmt.Errorf("feature flag entry %q has an empty key", entry)
		}
		if _, duplicate := out[key]; duplicate {
			return nil, fmt.Errorf("feature flag %q is listed more than once", key)
		}

		enabled := true
		if hasValue {
			value := strings.TrimSpace(valuePart)
			if value == "" {
				return nil, fmt.Errorf("feature flag %q has an empty value", key)
			}
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("feature flag %q has invalid boolean value %q", key, value)
			}
			enabled = parsed
		}
		out[key] = enabled
	}
	return out, nil
}

// ParseForceOff parses FEATURE_FLAGS_FORCE_OFF, a comma-separated list of flag
// keys that must evaluate to false regardless of any database or environment
// setting. Unknown keys are rejected by config validation, not here, so the
// parser stays useful for tests.
func ParseForceOff(raw string) (map[Key]struct{}, error) {
	out := make(map[Key]struct{})
	for _, entry := range strings.Split(raw, ",") {
		key := Key(strings.TrimSpace(entry))
		if key == "" {
			continue
		}
		if _, duplicate := out[key]; duplicate {
			return nil, fmt.Errorf("force-off flag %q is listed more than once", key)
		}
		out[key] = struct{}{}
	}
	return out, nil
}

// KnownKeysError returns a descriptive error when parsed contains a key that
// is not in the compile-time catalog. It is shared by config validation and
// tests.
func KnownKeysError(parsed map[Key]bool) error {
	for key := range parsed {
		if !IsKnown(key) {
			return fmt.Errorf("unknown feature flag %q", key)
		}
	}
	return nil
}

// KnownForceOffError returns a descriptive error when forceOff contains a key
// that is not in the compile-time catalog.
func KnownForceOffError(forceOff map[Key]struct{}) error {
	for key := range forceOff {
		if !IsKnown(key) {
			return fmt.Errorf("unknown force-off feature flag %q", key)
		}
	}
	return nil
}
