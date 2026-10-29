package database

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func TestApplyRuntimeTimeouts(t *testing.T) {
	poolCfg, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/db?sslmode=disable&search_path=public")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	dbCfg := config.DBConfig{
		StatementTimeout:                1500 * time.Millisecond,
		LockTimeout:                     250 * time.Millisecond,
		IdleInTransactionSessionTimeout: 2 * time.Second,
	}

	applyRuntimeTimeouts(poolCfg.ConnConfig, dbCfg)

	for key, want := range map[string]string{
		postgresStatementTimeout:         "1500",
		postgresLockTimeout:              "250",
		postgresIdleInTransactionTimeout: "2000",
	} {
		if got := poolCfg.ConnConfig.RuntimeParams[key]; got != want {
			t.Errorf("RuntimeParams[%q] = %q, want %q", key, got, want)
		}
	}
	if got := poolCfg.ConnConfig.RuntimeParams["search_path"]; got != "public" {
		t.Errorf("applyRuntimeTimeouts overwrote search_path: %q", got)
	}
}

func TestApplyRuntimeTimeoutsSkipsZeroValues(t *testing.T) {
	poolCfg, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/db?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	applyRuntimeTimeouts(poolCfg.ConnConfig, config.DBConfig{})
	applyRuntimeTimeouts(nil, config.DBConfig{StatementTimeout: time.Second})

	for _, key := range []string{postgresStatementTimeout, postgresLockTimeout, postgresIdleInTransactionTimeout} {
		if _, ok := poolCfg.ConnConfig.RuntimeParams[key]; ok {
			t.Errorf("zero timeout created RuntimeParams[%q]", key)
		}
	}
}

func TestApplyRuntimeTimeoutsRoundsPositiveSubMillisecondUp(t *testing.T) {
	poolCfg, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/db?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	applyRuntimeTimeouts(poolCfg.ConnConfig, config.DBConfig{
		StatementTimeout:                time.Nanosecond,
		LockTimeout:                     500 * time.Microsecond,
		IdleInTransactionSessionTimeout: time.Microsecond,
	})

	for _, key := range []string{postgresStatementTimeout, postgresLockTimeout, postgresIdleInTransactionTimeout} {
		if got := poolCfg.ConnConfig.RuntimeParams[key]; got != "1" {
			t.Errorf("RuntimeParams[%q] = %q, want %q", key, got, "1")
		}
	}
}
