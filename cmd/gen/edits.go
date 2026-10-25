package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// Markers are comment lines in generated (and bootstrap) files. The
// generator inserts new code immediately above them, so they must stay in
// place. Each marker must appear exactly once in its file.
const (
	markerHandlers    = "// gen:handlers"
	markerBuild       = "// gen:build"
	markerRoutes      = "// gen:routes"
	markerPermissions = "// gen:permissions"
	markerGrants      = "// gen:grants"
	markerCodes       = "// gen:codes"
	markerSpecs       = "// gen:specs"
	markerReadme      = "<!-- gen:slices -->"

	markerBootstrapBuild = "// gen:module-build"
	markerBootstrapList  = "// gen:module-list"
)

// insertAtMarker inserts block (one or more lines) above the line holding
// marker, indented like the marker. path names the file in errors.
func insertAtMarker(path, src, marker, block string) (string, error) {
	lines := strings.Split(src, "\n")
	at := -1
	for i, line := range lines {
		if strings.TrimSpace(line) != marker {
			continue
		}
		if at >= 0 {
			return "", fmt.Errorf("%s: marker %q appears more than once", path, marker)
		}
		at = i
	}
	if at < 0 {
		return "", fmt.Errorf("%s: marker %q not found; cmd/gen only extends code it generated (keep the gen: markers)", path, marker)
	}
	indent := lines[at][:len(lines[at])-len(strings.TrimLeft(lines[at], " \t"))]
	var inserted []string
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		if line == "" {
			inserted = append(inserted, "")
			continue
		}
		inserted = append(inserted, indent+line)
	}
	out := make([]string, 0, len(lines)+len(inserted))
	out = append(out, lines[:at]...)
	out = append(out, inserted...)
	out = append(out, lines[at:]...)
	return strings.Join(out, "\n"), nil
}

// ensureImports adds the missing import paths to a Go file that has a
// parenthesized import block. Standard-library paths join the first group
// and module paths the last group, matching the repository layout; gofmt then
// sorts each group.
func ensureImports(path, src string, imports ...string) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
	if err != nil {
		return "", fmt.Errorf("%s: parse imports: %w", path, err)
	}
	have := map[string]bool{}
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return "", fmt.Errorf("%s: import %s: %w", path, spec.Path.Value, err)
		}
		have[value] = true
	}
	var std, local []string
	for _, imp := range imports {
		if have[imp] {
			continue
		}
		have[imp] = true
		if isModuleImport(imp) {
			local = append(local, imp)
		} else {
			std = append(std, imp)
		}
	}
	if len(std) == 0 && len(local) == 0 {
		return src, nil
	}
	if len(file.Decls) == 0 || len(file.Imports) == 0 {
		return "", fmt.Errorf("%s: expected an existing import block", path)
	}
	firstImport := fset.Position(file.Imports[0].Pos()).Offset
	lastImport := fset.Position(file.Imports[len(file.Imports)-1].End()).Offset
	if !strings.Contains(src[:firstImport], "import (") {
		return "", fmt.Errorf("%s: imports must use a parenthesized block", path)
	}
	lastPath, err := strconv.Unquote(file.Imports[len(file.Imports)-1].Path.Value)
	if err != nil {
		return "", fmt.Errorf("%s: last import: %w", path, err)
	}
	var b strings.Builder
	b.WriteString(src[:firstImport])
	for _, imp := range std {
		b.WriteString(strconv.Quote(imp) + "\n\t")
	}
	b.WriteString(src[firstImport:lastImport])
	if len(local) > 0 && !isModuleImport(lastPath) {
		// Open the module group after a stdlib-only block.
		b.WriteString("\n")
	}
	for _, imp := range local {
		b.WriteString("\n\t" + strconv.Quote(imp))
	}
	b.WriteString(src[lastImport:])
	return b.String(), nil
}

// isModuleImport reports whether imp is outside the standard library (its
// first path element has a dot, like example.com/...).
func isModuleImport(imp string) bool {
	return strings.Contains(strings.SplitN(imp, "/", 2)[0], ".")
}

// formatGo gofmt-formats generated Go source. A failure is a generator bug,
// so the error carries the source for diagnosis.
func formatGo(path string, src []byte) ([]byte, error) {
	out, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf("gofmt %s: %w\n%s", path, err, src)
	}
	return out, nil
}

// mergeLocale adds entries to a flat JSON locale file and rewrites it with
// sorted keys. An existing key is an error: catalog keys are public API and
// must not be silently replaced.
func mergeLocale(path string, src []byte, entries map[string]string) ([]byte, error) {
	values := map[string]string{}
	if err := json.Unmarshal(src, &values); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for key, value := range entries {
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("%s: key %q already exists", path, key)
		}
		values[key] = value
	}
	return marshalLocale(values)
}

// marshalLocale renders a flat locale map with sorted keys, two-space
// indentation, no HTML escaping, and a trailing newline.
func marshalLocale(values map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, key := range keys {
		k, err := marshalString(key)
		if err != nil {
			return nil, err
		}
		v, err := marshalString(values[key])
		if err != nil {
			return nil, err
		}
		b.WriteString("  " + k + ": " + v)
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

func marshalString(s string) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
