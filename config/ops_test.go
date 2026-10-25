package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return path
}

func TestLoad_ReadsSecretsFromFiles(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	t.Setenv("JWT_SECRET_FILE", writeSecretFile(t, "file-jwt-secret-at-least-32-characters\n"))
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_PASSWORD_FILE", writeSecretFile(t, "db pass with spaces \r\n"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.JWT.Secret != "file-jwt-secret-at-least-32-characters" {
		t.Errorf("JWT secret = %q, want the file content without its trailing newline", cfg.JWT.Secret)
	}
	if cfg.DB.Password != "db pass with spaces " {
		t.Errorf("DB password = %q, want inner/trailing spaces kept and only the line break trimmed", cfg.DB.Password)
	}

	db, err := LoadDatabase()
	if err != nil {
		t.Fatalf("LoadDatabase() error: %v", err)
	}
	if db.Password != "db pass with spaces " {
		t.Errorf("LoadDatabase password = %q, want the _FILE value", db.Password)
	}
}

func TestLoad_SecretFileErrors(t *testing.T) {
	t.Run("both forms set", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "inline")
		t.Setenv("JWT_SECRET_FILE", writeSecretFile(t, "from-file"))
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "JWT_SECRET or JWT_SECRET_FILE") {
			t.Fatalf("Load() error = %v, want an ambiguity error", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("DB_PASSWORD", "")
		t.Setenv("DB_PASSWORD_FILE", filepath.Join(t.TempDir(), "absent"))
		if _, err := LoadDatabase(); err == nil || !strings.Contains(err.Error(), "DB_PASSWORD_FILE") {
			t.Fatalf("LoadDatabase() error = %v, want a read error naming DB_PASSWORD_FILE", err)
		}
	})
}

func TestValidate_JWTPreviousSecrets(t *testing.T) {
	cfg := productionValidConfig()
	cfg.JWT.PreviousSecrets = strings.Repeat("p", 32) + ", " + strings.Repeat("q", 32)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid previous secrets rejected: %v", err)
	}
	if got := cfg.JWT.PreviousSecretList(); len(got) != 2 {
		t.Fatalf("PreviousSecretList() = %d entries, want 2", len(got))
	}

	cfg.JWT.PreviousSecrets = "short"
	if err := cfg.Validate(); err == nil {
		t.Error("a short previous JWT secret must be rejected outside local/test")
	}

	cfg.JWT.PreviousSecrets = cfg.JWT.Secret
	if err := cfg.Validate(); err == nil {
		t.Error("the active JWT secret must not be listed as a previous secret")
	}
}

func TestValidate_DataEncryptionKey(t *testing.T) {
	t.Run("required in production", func(t *testing.T) {
		cfg := productionValidConfig()
		cfg.DataEncryption.Key = ""
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "DATA_ENCRYPTION_KEY") {
			t.Fatalf("Validate() = %v, want DATA_ENCRYPTION_KEY required", err)
		}
	})
	t.Run("must differ from JWT secret", func(t *testing.T) {
		cfg := productionValidConfig()
		cfg.DataEncryption.Key = cfg.JWT.Secret
		if err := cfg.Validate(); err == nil {
			t.Fatal("DATA_ENCRYPTION_KEY equal to JWT_SECRET must be rejected in production")
		}
	})
	t.Run("short key", func(t *testing.T) {
		cfg := baseValidConfig()
		cfg.DataEncryption.Key = "short"
		if err := cfg.Validate(); err == nil {
			t.Fatal("a short DATA_ENCRYPTION_KEY must be rejected")
		}
	})
	t.Run("local falls back to JWT secret", func(t *testing.T) {
		cfg := baseValidConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("local config without DATA_ENCRYPTION_KEY: %v", err)
		}
		if got := cfg.DataEncryption.ResolvedKey(cfg.JWT.Secret); got != cfg.JWT.Secret {
			t.Fatalf("ResolvedKey() = %q, want the JWT secret fallback", got)
		}
	})
	t.Run("previous keys", func(t *testing.T) {
		cfg := productionValidConfig()
		// The old JWT secret is the expected previous key when migrating.
		cfg.DataEncryption.PreviousKeys = cfg.JWT.Secret
		if err := cfg.Validate(); err != nil {
			t.Fatalf("JWT secret as previous data key rejected: %v", err)
		}
		cfg.DataEncryption.PreviousKeys = cfg.DataEncryption.Key
		if err := cfg.Validate(); err == nil {
			t.Fatal("the active data key must not be listed as a previous key")
		}
		cfg.DataEncryption.PreviousKeys = "short"
		if err := cfg.Validate(); err == nil {
			t.Fatal("a short previous data key must be rejected outside local/test")
		}
	})
}

func TestValidate_WebhookPreviousKeys(t *testing.T) {
	cfg := productionValidConfig()
	cfg.Webhook.SecretEncryptionPreviousKeys = strings.Repeat("o", 32)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid previous webhook key rejected: %v", err)
	}
	cfg.Webhook.SecretEncryptionPreviousKeys = cfg.Webhook.SecretEncryptionKey
	if err := cfg.Validate(); err == nil {
		t.Fatal("the active webhook key must not be listed as a previous key")
	}
	cfg.Webhook.SecretEncryptionPreviousKeys = "short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a short previous webhook key must be rejected outside local/test")
	}
}

func TestAppRole(t *testing.T) {
	tests := []struct {
		role              string
		servesAPI, worker bool
	}{
		{role: "", servesAPI: true, worker: true},
		{role: RoleAll, servesAPI: true, worker: true},
		{role: RoleAPI, servesAPI: true, worker: false},
		{role: RoleWorker, servesAPI: false, worker: true},
	}
	for _, tt := range tests {
		cfg := baseValidConfig()
		cfg.App.Role = tt.role
		if err := cfg.Validate(); err != nil {
			t.Fatalf("APP_ROLE=%q rejected: %v", tt.role, err)
		}
		if cfg.App.ServesAPI() != tt.servesAPI || cfg.App.RunsWorker() != tt.worker {
			t.Errorf("APP_ROLE=%q: ServesAPI=%v RunsWorker=%v", tt.role, cfg.App.ServesAPI(), cfg.App.RunsWorker())
		}
	}

	cfg := baseValidConfig()
	cfg.App.Role = "scheduler"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown APP_ROLE must be rejected")
	}
}

func TestJobsConfig(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Jobs = JobsConfig{WorkerKinds: "webhook.delivery, events.file.uploaded.webhooks", WorkerExcludeKinds: "email.send"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disjoint kind filters rejected: %v", err)
	}
	if got := cfg.Jobs.WorkerKindList(); len(got) != 2 || got[1] != "events.file.uploaded.webhooks" {
		t.Fatalf("WorkerKindList() = %v", got)
	}

	cfg.Jobs.WorkerExcludeKinds = "webhook.delivery"
	if err := cfg.Validate(); err == nil {
		t.Fatal("a kind both included and excluded must be rejected")
	}
}

func TestDBConfig_QueryExecModeAndMigrationTarget(t *testing.T) {
	cfg := baseValidConfig()
	if got := cfg.DB.EffectiveQueryExecMode(); got != "cache_statement" {
		t.Fatalf("default exec mode = %q", got)
	}
	cfg.DB.QueryExecMode = "cache_describe"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cache_describe rejected: %v", err)
	}
	// exec/simple_protocol do not describe parameter types and break json
	// arguments; "prepared" is not a pgx mode at all.
	for _, mode := range []string{"exec", "simple_protocol", "prepared"} {
		cfg.DB.QueryExecMode = mode
		if err := cfg.Validate(); err == nil {
			t.Fatalf("DB_QUERY_EXEC_MODE=%s must be rejected", mode)
		}
	}

	db := DBConfig{Host: "pgbouncer", Port: 6432}
	if got := db.ForMigration(); got.Host != "pgbouncer" || got.Port != 6432 {
		t.Fatalf("ForMigration() without overrides = %s:%d", got.Host, got.Port)
	}
	db.MigrateHost, db.MigratePort = "postgres-primary", 5432
	if got := db.ForMigration(); got.Host != "postgres-primary" || got.Port != 5432 {
		t.Fatalf("ForMigration() = %s:%d, want the DB_MIGRATE_* target", got.Host, got.Port)
	}
	if db.Host != "pgbouncer" {
		t.Fatal("ForMigration must not modify the API settings")
	}

	db = DBConfig{Host: "db", Username: "app", Password: "app-password"}
	db.MigrateUsername, db.MigratePassword = "owner", "owner-password"
	got := db.ForMigration()
	if got.Username != "owner" || got.Password != "owner-password" {
		t.Fatalf("ForMigration() credentials = %q/%q, want the DB_MIGRATE_* role", got.Username, got.Password)
	}
	if db.Username != "app" || db.Password != "app-password" {
		t.Fatal("ForMigration must not modify the API credentials")
	}
}
