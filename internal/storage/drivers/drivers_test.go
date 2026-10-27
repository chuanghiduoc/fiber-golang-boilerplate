package drivers

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func TestNew_Local(t *testing.T) {
	s, err := New(config.StorageConfig{Driver: "local", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatalf("New(local) failed: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Storage")
	}
}

func TestNew_Unsupported(t *testing.T) {
	_, err := New(config.StorageConfig{Driver: "gcs"})
	if err == nil {
		t.Error("New should fail for unsupported driver")
	}
}
