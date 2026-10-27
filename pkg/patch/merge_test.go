package patch_test

import (
	"encoding/json"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

type update struct {
	Name  patch.Field[string] `json:"name"`
	Email patch.Field[string] `json:"email"`
}

func TestFieldDistinguishesAbsentNullAndValue(t *testing.T) {
	cases := map[string]struct {
		body       string
		name       string
		present    bool
		null       bool
		afterApply string
	}{
		"absent leaves target":  {body: `{}`, name: "", present: false, afterApply: "original"},
		"null clears target":    {body: `{"name":null}`, name: "", present: true, null: true, afterApply: ""},
		"value replaces target": {body: `{"name":"new"}`, name: "new", present: true, afterApply: "new"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var in update
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if in.Name.Present != tc.present || in.Name.Null != tc.null || in.Name.Value != tc.name {
				t.Fatalf("field = %+v, want present=%v null=%v value=%q", in.Name, tc.present, tc.null, tc.name)
			}
			target := "original"
			in.Name.Apply(&target)
			if target != tc.afterApply {
				t.Fatalf("after apply = %q, want %q", target, tc.afterApply)
			}
		})
	}
}

func TestMergeImplementsRFC7396(t *testing.T) {
	target := json.RawMessage(`{"name":"old","email":"a@example.com","nested":{"keep":1,"drop":2}}`)
	patchJSON := json.RawMessage(`{"name":"new","email":null,"nested":{"drop":null,"add":3}}`)
	got, err := patch.Merge(target, patchJSON)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body["name"] != "new" {
		t.Errorf("name = %v, want new", body["name"])
	}
	if _, ok := body["email"]; ok {
		t.Errorf("email must be removed by null, got %v", body["email"])
	}
	nested := body["nested"].(map[string]any)
	if nested["keep"] != float64(1) || nested["add"] != float64(3) {
		t.Errorf("nested = %v", nested)
	}
	if _, ok := nested["drop"]; ok {
		t.Errorf("nested.drop must be removed, got %v", nested["drop"])
	}
}
