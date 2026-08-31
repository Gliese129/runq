package utils

import (
	"path/filepath"
	"testing"
)

func TestRunqdSocketPathUsesIndependentEnvironment(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", filepath.Join(t.TempDir(), "runq-lab"))
	runqdData := filepath.Join(t.TempDir(), "runqd")
	t.Setenv("RUNQD_DATA_DIR", runqdData)
	t.Setenv("RUNQD_SOCKET", "")
	if got, want := RunqdSocketPath(), filepath.Join(runqdData, "runqd.sock"); got != want {
		t.Fatalf("runqd socket = %q, want %q", got, want)
	}

	override := filepath.Join(t.TempDir(), "custom.sock")
	t.Setenv("RUNQD_SOCKET", override)
	if got := RunqdSocketPath(); got != override {
		t.Fatalf("RUNQD_SOCKET override = %q, want %q", got, override)
	}
}
