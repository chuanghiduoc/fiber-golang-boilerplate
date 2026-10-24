package i18n

import (
	"strings"
	"testing"
	"testing/fstest"
)

func moduleFS(en, vi string) fstest.MapFS {
	return fstest.MapFS{
		"en.json": {Data: []byte(en)},
		"vi.json": {Data: []byte(vi)},
	}
}

func TestNewMergesModuleSources(t *testing.T) {
	bundle, err := New("en", Source{Owner: "widgets", FS: moduleFS(
		`{"widgets.created": "Widget {name} created", "errors.widget.locked.title": "Widget locked"}`,
		`{"widgets.created": "Đã tạo {name}", "errors.widget.locked.title": "Widget bị khóa"}`,
	)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := bundle.Translate("vi", "widgets.created", map[string]any{"name": "A"}); got != "Đã tạo A" {
		t.Fatalf("Translate vi = %q", got)
	}
	if got := bundle.Translate("en", "errors.widget.locked.title", nil); got != "Widget locked" {
		t.Fatalf("Translate en = %q", got)
	}
	// Platform keys survive the merge.
	if !bundle.Has("vi", "validation.required") {
		t.Fatal("platform catalog lost after merge")
	}
}

func TestNewRejectsBadModuleSources(t *testing.T) {
	cases := map[string]struct {
		sources []Source
		want    string
	}{
		"missing locale": {[]Source{{Owner: "widgets", FS: fstest.MapFS{
			"en.json": {Data: []byte(`{"widgets.a": "A"}`)},
		}}}, "missing vi.json"},
		"extra locale": {[]Source{{Owner: "widgets", FS: fstest.MapFS{
			"en.json": {Data: []byte(`{"widgets.a": "A"}`)},
			"vi.json": {Data: []byte(`{"widgets.a": "A"}`)},
			"fr.json": {Data: []byte(`{"widgets.a": "A"}`)},
		}}}, "unsupported locale"},
		"parity": {[]Source{{Owner: "widgets", FS: moduleFS(
			`{"widgets.a": "A", "widgets.b": "B"}`, `{"widgets.a": "A"}`,
		)}}, "does not match"},
		"placeholders": {[]Source{{Owner: "widgets", FS: moduleFS(
			`{"widgets.a": "Hi {name}"}`, `{"widgets.a": "Xin chào"}`,
		)}}, "placeholders"},
		"namespace": {[]Source{{Owner: "widgets", FS: moduleFS(
			`{"orders.a": "A"}`, `{"orders.a": "A"}`,
		)}}, "must start with"},
		"platform collision": {[]Source{{Owner: "widgets", FS: moduleFS(
			`{"errors.permission.denied.title": "X"}`, `{"errors.permission.denied.title": "X"}`,
		)}}, "already defined"},
		"module collision": {[]Source{
			{Owner: "widgets", FS: moduleFS(`{"errors.shared.title": "A"}`, `{"errors.shared.title": "A"}`)},
			{Owner: "gadgets", FS: moduleFS(`{"errors.shared.title": "B"}`, `{"errors.shared.title": "B"}`)},
		}, "already defined"},
		"bad json": {[]Source{{Owner: "widgets", FS: moduleFS(`{`, `{}`)}}, "decode"},
		"no owner": {[]Source{{FS: moduleFS(`{}`, `{}`)}}, "owner"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New("en", tc.sources...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// TestModuleSourceDoesNotMutateEmbeddedCatalogs guards against a merge that
// writes into shared state: a later bundle without sources must not see
// module keys.
func TestModuleSourceDoesNotMutateEmbeddedCatalogs(t *testing.T) {
	if _, err := New("en", Source{Owner: "widgets", FS: moduleFS(`{"widgets.a": "A"}`, `{"widgets.a": "A"}`)}); err != nil {
		t.Fatal(err)
	}
	plain, err := New("en")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Has("en", "widgets.a") {
		t.Fatal("module keys leaked into a bundle built without sources")
	}
}
