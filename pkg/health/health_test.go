package health

import (
	"testing"
)

func TestLiveness(t *testing.T) {
	checker := &Checker{}
	status := checker.Liveness()

	if status.Status != "up" {
		t.Errorf("Liveness().Status = %q, want up", status.Status)
	}
}

func TestNewChecker(t *testing.T) {
	checker := NewChecker(nil, nil)
	if checker == nil {
		t.Fatal("NewChecker returned nil")
	}
	if checker.pool != nil {
		t.Error("expected nil pool")
	}
}

func TestNewChecker_Liveness(t *testing.T) {
	checker := NewChecker(nil, nil)
	status := checker.Liveness()
	if status.Status != "up" {
		t.Errorf("Liveness().Status = %q, want up", status.Status)
	}
}
