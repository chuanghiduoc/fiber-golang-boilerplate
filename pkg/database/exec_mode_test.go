package database

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// config validates DB_QUERY_EXEC_MODE names independently; every mapped mode
// must pass that validation so the two lists cannot drift apart silently.
func TestQueryExecModesMatchConfigValidation(t *testing.T) {
	for name := range QueryExecModes {
		db := config.DBConfig{ConnectTimeout: 1, MaxConns: 1, QueryExecMode: name}
		if err := db.Validate(); err != nil {
			t.Errorf("config rejects mapped exec mode %q: %v", name, err)
		}
	}
	if _, ok := QueryExecModes[config.DBConfig{}.EffectiveQueryExecMode()]; !ok {
		t.Fatal("the default exec mode must be mapped")
	}
}
