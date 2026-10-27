// Package patch implements the subset of RFC 7396 JSON Merge Patch that HTTP
// handlers need: an explicitly null member clears a field, an absent member
// leaves it unchanged, and any other value replaces it.
//
// A request DTO uses Nullable[T] for a field that may be cleared. The zero value
// means "absent"; UnmarshalJSON records both presence and null.
package patch

import (
	"bytes"
	"encoding/json"
)

// Field is one optionally-present JSON member in a merge-patch input.
//
// Present reports whether the member appeared in the request at all. Null
// reports an explicit JSON null (clear the target). Value holds the decoded
// replacement when Present && !Null.
type Field[T any] struct {
	Present bool
	Null    bool
	Value   T
}

// UnmarshalJSON records that the member was present and whether it was null.
// An empty document is an error: callers only decode members that exist.
func (f *Field[T]) UnmarshalJSON(data []byte) error {
	f.Present = true
	f.Null = false
	var zero T
	f.Value = zero
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		f.Null = true
		return nil
	}
	return json.Unmarshal(data, &f.Value)
}

// Apply writes the field to target when it was present. A null clears the
// target to its zero value. Absent fields leave target untouched.
func (f Field[T]) Apply(target *T) {
	if !f.Present {
		return
	}
	if f.Null {
		var zero T
		*target = zero
		return
	}
	*target = f.Value
}

// Merge applies an RFC 7396 merge patch to a JSON object and returns the
// resulting object. It is the escape hatch for free-form objects (for example
// notification preferences); typed DTOs should use Field[T] instead.
//
// The input documents must be JSON objects. Null members are deleted, absent
// members are preserved, and object values are merged recursively.
func Merge(target, patch json.RawMessage) (json.RawMessage, error) {
	var targetObj map[string]json.RawMessage
	if len(bytes.TrimSpace(target)) == 0 {
		targetObj = map[string]json.RawMessage{}
	} else if err := json.Unmarshal(target, &targetObj); err != nil {
		return nil, err
	}
	var patchObj map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchObj); err != nil {
		return nil, err
	}
	merged, err := mergeObject(targetObj, patchObj)
	if err != nil {
		return nil, err
	}
	return json.Marshal(merged)
}

func mergeObject(target, patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	// Sized by target only: patch keys may already be present, and summing two
	// lengths in an allocation size is an overflow CodeQL rightly flags.
	out := make(map[string]json.RawMessage, len(target))
	for key, value := range target {
		out[key] = value
	}
	for key, value := range patch {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			delete(out, key)
			continue
		}
		existing, hasExisting := out[key]
		var targetChild, patchChild map[string]json.RawMessage
		if hasExisting && json.Unmarshal(existing, &targetChild) == nil {
			if json.Unmarshal(value, &patchChild) == nil && targetChild != nil && patchChild != nil {
				child, err := mergeObject(targetChild, patchChild)
				if err != nil {
					return nil, err
				}
				encoded, err := json.Marshal(child)
				if err != nil {
					return nil, err
				}
				out[key] = encoded
				continue
			}
		}
		out[key] = value
	}
	return out, nil
}
