package remote

import (
	"os"
	"testing"
)

// TestMain points RUNQ_DATA_DIR at a throwaway dir so best-effort side
// channels (the operation log) never touch the developer's real ~/.runq.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "runq-hpc-test")
	if err == nil {
		os.Setenv("RUNQ_DATA_DIR", dir)
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
