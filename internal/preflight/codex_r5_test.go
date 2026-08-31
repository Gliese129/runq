package preflight

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/rfs"
)

// Codex r5: timeout-side cleanup used to RACE the background upload —
// removeProbe ran immediately at the deadline, before the slow write
// had created the file, so the delete was a no-op and the late write
// re-materialized .runq_preflight_*.py. Cleanup must be sequenced
// AFTER the background write completes.
func TestR5LateWriteCleanupNotRaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "probe.py")
	slow := slowWriteFS{FS: rfs.NewLocalFS(), delay: 250 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	cleaned := make(chan struct{})
	start := time.Now()
	err := writeFileCtx(ctx, slow, p, []byte("x"), 0o644, func() {
		_ = os.Remove(p)
		close(cleaned)
	})
	if err == nil {
		t.Fatal("deadline not enforced")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("writeFileCtx held past deadline: %s", elapsed)
	}

	// The write is still running in the background; the file WILL land.
	// The old immediate-cleanup sequencing left it there forever.
	select {
	case <-cleaned:
	case <-time.After(3 * time.Second):
		t.Fatal("late-write cleanup never ran")
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Fatalf("probe file left behind after late write: stat err=%v", statErr)
	}
}

// A write that completes WITHIN the deadline must not trigger the late
// cleanup path — the probe file is needed by the exec that follows.
func TestR5FastWriteSkipsLateCleanup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "probe.py")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fired := make(chan struct{}, 1)
	if err := writeFileCtx(ctx, rfs.NewLocalFS(), p, []byte("x"), 0o644, func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-fired:
		t.Fatal("late cleanup fired on an in-deadline write")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("probe file missing after successful write: %v", err)
	}
}
