package arch_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestProductionBinariesDoNotLoadDotEnv guards the deployment boundary: .env
// loading is a local-development convenience behind the `dev` build tag, and a
// release binary must take its configuration from the environment only.
func TestProductionBinariesDoNotLoadDotEnv(t *testing.T) {
	root := repoRoot(t)
	for _, pkg := range []string{"./cmd/api", "./cmd/migrate", "./cmd/seed"} {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pkg)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, imp := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(imp, "github.com/joho/godotenv") {
				t.Errorf("%s links %s without the dev tag; load .env only in the dev entrypoint", pkg, imp)
			}
		}
	}
}
