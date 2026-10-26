package arch_test

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// layerRules is the application layer (service/) and the HTTP
// adapter (handler/) of a module must not reach into persistence or
// transport details. Each rule lists import-path fragments the layer may not
// import.
var layerRules = map[string][]layerBan{
	"service": {
		{fragment: "/sqlc", reason: "map sqlc rows to service types in repository/"},
		{fragment: "github.com/jackc/pgx", reason: "transactions go through a port, not pgx.Tx"},
		{fragment: "github.com/gofiber/fiber", reason: "the application layer is transport-agnostic"},
		{fragment: "/dto", reason: "return service types; handler/ maps them to DTOs"},
		{fragment: "/handler", reason: "dependencies point inward"},
	},
	"handler": {
		{fragment: "/repository", reason: "handlers call services, never repositories"},
		{fragment: "/sqlc", reason: "handlers never see persistence rows"},
		{fragment: "github.com/jackc/pgx", reason: "handlers never manage transactions"},
	},
}

type layerBan struct {
	fragment string
	reason   string
}

// layeringAllowlist holds the violations that existed when the rule was
// introduced. It is a ratchet: a new violation fails, and an entry whose
// violation is gone must be deleted so the list only shrinks.
const layeringAllowlist = "layering_allowlist.txt"

func TestModuleLayering(t *testing.T) {
	root := repoRoot(t)
	modulesDir := filepath.Join(root, "internal", "modules")
	const modulePrefix = "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/"

	found := map[string]string{}
	err := filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(modulesDir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 {
			return nil
		}
		module, layer := parts[0], parts[1]
		bans, ok := layerRules[layer]
		if !ok {
			return nil
		}
		for _, imp := range parseImports(t, path) {
			for _, ban := range bans {
				if !layerImportMatches(imp, ban.fragment, modulePrefix+module) {
					continue
				}
				key := filepath.ToSlash(rel) + " -> " + imp
				found[key] = ban.reason
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk modules: %v", err)
	}

	allowed := readLayeringAllowlist(t, filepath.Join(root, "test", "arch", layeringAllowlist))
	var fresh []string
	for key, reason := range found {
		if !allowed[key] {
			fresh = append(fresh, key+" ("+reason+")")
		}
	}
	var stale []string
	for key := range allowed {
		if _, ok := found[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(fresh)
	sort.Strings(stale)
	for _, v := range fresh {
		t.Errorf("layering violation: %s", v)
	}
	for _, v := range stale {
		t.Errorf("stale layering allowlist entry (violation fixed, delete it from %s): %s", layeringAllowlist, v)
	}
}

// layerImportMatches reports whether imp hits a ban. Module-internal
// fragments (/sqlc, /dto, /handler, /repository) only count inside the
// file's own module; third-party fragments match anywhere.
func layerImportMatches(imp, fragment, modulePath string) bool {
	if strings.HasPrefix(fragment, "/") {
		return strings.HasPrefix(imp, modulePath+fragment)
	}
	return strings.HasPrefix(imp, fragment)
}

func readLayeringAllowlist(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	allowed := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		allowed[line] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return allowed
}
