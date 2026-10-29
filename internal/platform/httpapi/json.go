package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// UnmarshalJSONStrict decodes exactly one JSON value and rejects unknown
// object members and trailing values. The API's request contract says unknown
// fields are errors; huma's default json.Unmarshal silently ignores them.
//
// Maps and json.RawMessage values still accept arbitrary keys, so endpoints
// with intentionally free-form bodies are unaffected.
func UnmarshalJSONStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains more than one JSON value")
		}
		return err
	}
	return nil
}
