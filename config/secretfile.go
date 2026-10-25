package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"
)

// secretFileSuffix is appended to a secret variable's name to read its value
// from a file instead (Docker/Kubernetes secret mounts):
// JWT_SECRET_FILE=/run/secrets/jwt sets JWT_SECRET from that file.
//
// caarlos0/env's own `file` tag option is not used: it reinterprets the
// variable itself as a path, which would change what JWT_SECRET=<value>
// means for every existing deployment. A separate _FILE variable is the
// common convention and keeps the plain variable working unchanged.
const secretFileSuffix = "_FILE"

// secretFileVars are the settings that accept <NAME>_FILE.
var secretFileVars = []string{
	"JWT_SECRET",
	"JWT_PREVIOUS_SECRETS",
	"DB_PASSWORD",
	"DB_MIGRATE_PASSWORD",
	"DATA_ENCRYPTION_KEY",
	"DATA_ENCRYPTION_PREVIOUS_KEYS",
	"WEBHOOK_SECRET_ENCRYPTION_KEY",
	"WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS",
	"SMTP_PASSWORD",
	"GOOGLE_CLIENT_SECRET",
	"ADMIN_PASSWORD",
	"METRICS_AUTH_TOKEN",
	"STORAGE_S3_SECRET_KEY",
	"REDIS_URL",
}

// loadEnvironment returns the process environment with every <NAME>_FILE
// secret resolved into <NAME>. Setting both forms is rejected so it is never
// ambiguous which value is in effect. Trailing line breaks are trimmed
// because most tools that write secret files add one; other whitespace is
// preserved as part of the secret.
func loadEnvironment() (map[string]string, error) {
	environment := env.ToMap(os.Environ())
	for _, name := range secretFileVars {
		path := strings.TrimSpace(environment[name+secretFileSuffix])
		if path == "" {
			continue
		}
		if environment[name] != "" {
			return nil, fmt.Errorf("set either %s or %s%s, not both", name, name, secretFileSuffix)
		}
		// #nosec G304 -- the path is operator configuration, like the value it replaces.
		content, err := os.ReadFile(path)
		if err != nil {
			// The path is configuration, not a secret; the content is never logged.
			return nil, fmt.Errorf("read %s%s: %w", name, secretFileSuffix, err)
		}
		environment[name] = strings.TrimRight(string(content), "\r\n")
	}
	return environment, nil
}
