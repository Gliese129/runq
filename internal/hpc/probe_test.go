package hpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/gliese129/runq/internal/hpcconfig"
)

func TestProbeScheduler(t *testing.T) {
	ctx := context.Background()

	// constRunner returns the same output for any command.
	constRunner := func(out string, err error) Runner {
		return func(ctx context.Context, command string) (string, error) { return out, err }
	}

	t.Run("no status_template", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{}, Run: constRunner("RUNNING", nil)}
		if got := b.probeScheduler(ctx, "1"); got != SchedUnknown {
			t.Fatalf("got %q, want unknown", got)
		}
	})

	t.Run("no ext id", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "squeue {{ext_id}}"}, Run: constRunner("RUNNING", nil)}
		if got := b.probeScheduler(ctx, ""); got != SchedUnknown {
			t.Fatalf("got %q, want unknown", got)
		}
	})

	t.Run("probe error", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "squeue {{ext_id}}"}, Run: constRunner("", fmt.Errorf("boom"))}
		if got := b.probeScheduler(ctx, "1"); got != SchedUnknown {
			t.Fatalf("got %q, want unknown", got)
		}
	})

	t.Run("empty output is gone", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "squeue {{ext_id}}"}, Run: constRunner("", nil)}
		if got := b.probeScheduler(ctx, "1"); got != SchedGone {
			t.Fatalf("got %q, want gone", got)
		}
	})

	t.Run("recognized token", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "sacct {{ext_id}}"}, Run: constRunner("FAILED\n", nil)}
		if got := b.probeScheduler(ctx, "1"); got != SchedFailed {
			t.Fatalf("got %q, want failed", got)
		}
	})

	t.Run("present but unrecognized is active", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "qstat {{ext_id}}"}, Run: constRunner("R", nil)}
		if got := b.probeScheduler(ctx, "1"); got != SchedActive {
			t.Fatalf("got %q, want active", got)
		}
	})

	// The parser pipeline tests use the REAL shell runner so the pipe assembly
	// (raw output → stdin of stage 1 → piped through each stage) is exercised end
	// to end, the way it runs on a login node.
	t.Run("parser pipeline single stage", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo R",
			StatusParser:   []string{"sed s/R/running/"},
		}, Run: shellRunner}
		if got := b.probeScheduler(ctx, "1"); got != SchedRunning {
			t.Fatalf("got %q, want running", got)
		}
	})

	t.Run("parser pipeline multi stage (PBS-style)", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo 'job_state = R'",
			StatusParser: []string{
				"grep -o 'job_state = .'",
				"awk '{print $3}'",
				"sed s/R/running/",
			},
		}, Run: shellRunner}
		if got := b.probeScheduler(ctx, "1"); got != SchedRunning {
			t.Fatalf("got %q, want running", got)
		}
	})

	t.Run("parser pipeline uses ext_id stage", func(t *testing.T) {
		// A stage may reference {{ext_id}}; it is shell-escaped by Render.
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo ignored",
			StatusParser:   []string{"grep -q {{ext_id}} && echo running || echo gone"},
		}, Run: shellRunner}
		if got := b.probeScheduler(ctx, "ignored"); got != SchedRunning {
			t.Fatalf("got %q, want running", got)
		}
	})

	t.Run("parser empty output (exit 0) is gone", func(t *testing.T) {
		// Job vanished from the active query: the parser exits 0 with no token.
		// This must map to gone (not unknown), else a dead task zombies as running.
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo R",
			StatusParser:   []string{"awk '/NOPE/{print}'"}, // matches nothing, exit 0, empty
		}, Run: shellRunner}
		if got := b.probeScheduler(ctx, "1"); got != SchedGone {
			t.Fatalf("got %q, want gone", got)
		}
	})
}
