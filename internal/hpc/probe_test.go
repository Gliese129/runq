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
		if got := b.probeScheduler(ctx, "1"); got.Signal != SchedUnknown {
			t.Fatalf("got %q, want unknown", got.Signal)
		}
	})

	t.Run("no ext id", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "squeue {{ext_id}}"}, Run: constRunner("RUNNING", nil)}
		if got := b.probeScheduler(ctx, ""); got.Signal != SchedUnknown {
			t.Fatalf("got %q, want unknown", got.Signal)
		}
	})

	t.Run("probe error", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "squeue {{ext_id}}"}, Run: constRunner("", fmt.Errorf("boom"))}
		if got := b.probeScheduler(ctx, "1"); got.Signal != SchedUnknown {
			t.Fatalf("got %q, want unknown", got.Signal)
		}
	})

	t.Run("empty output is gone", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "squeue {{ext_id}}"}, Run: constRunner("", nil)}
		if got := b.probeScheduler(ctx, "1"); got.Signal != SchedGone {
			t.Fatalf("got %q, want gone", got.Signal)
		}
	})

	t.Run("recognized token", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "sacct {{ext_id}}"}, Run: constRunner("FAILED\n", nil)}
		got := b.probeScheduler(ctx, "1")
		if got.Signal != SchedFailed {
			t.Fatalf("got signal %q, want failed", got.Signal)
		}
		if got.NativeState != "FAILED" {
			t.Fatalf("got native_state %q, want FAILED", got.NativeState)
		}
	})

	t.Run("present but unrecognized is active", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{StatusTemplate: "qstat {{ext_id}}"}, Run: constRunner("R", nil)}
		got := b.probeScheduler(ctx, "1")
		if got.Signal != SchedActive {
			t.Fatalf("got %q, want active", got.Signal)
		}
		if got.NativeState != "R" {
			t.Fatalf("got native_state %q, want R", got.NativeState)
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
		if got := b.probeScheduler(ctx, "1"); got.Signal != SchedRunning {
			t.Fatalf("got %q, want running", got.Signal)
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
		if got := b.probeScheduler(ctx, "1"); got.Signal != SchedRunning {
			t.Fatalf("got %q, want running", got.Signal)
		}
	})

	t.Run("parser pipeline uses ext_id stage", func(t *testing.T) {
		// A stage may reference {{ext_id}}; it is shell-escaped by Render.
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo ignored",
			StatusParser:   []string{"grep -q {{ext_id}} && echo running || echo gone"},
		}, Run: shellRunner}
		if got := b.probeScheduler(ctx, "ignored"); got.Signal != SchedRunning {
			t.Fatalf("got %q, want running", got.Signal)
		}
	})

	t.Run("parser empty output (exit 0) is gone", func(t *testing.T) {
		// Job vanished from the active query: the parser exits 0 with no token.
		// This must map to gone (not unknown), else a dead task zombies as running.
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo R",
			StatusParser:   []string{"awk '/NOPE/{print}'"}, // matches nothing, exit 0, empty
		}, Run: shellRunner}
		if got := b.probeScheduler(ctx, "1"); got.Signal != SchedGone {
			t.Fatalf("got %q, want gone", got.Signal)
		}
	})

	t.Run("signal_map overrides unknown token", func(t *testing.T) {
		b := &Backend{Cfg: &hpcconfig.Config{
			StatusTemplate: "echo {{ext_id}}",
			SignalMap:       map[string]string{"CONFIGURING": "pending"},
		}, Run: constRunner("CONFIGURING", nil)}
		got := b.probeScheduler(ctx, "1")
		if got.Signal != SchedPending {
			t.Fatalf("got signal %q, want pending", got.Signal)
		}
		if got.NativeState != "CONFIGURING" {
			t.Fatalf("got native_state %q, want CONFIGURING", got.NativeState)
		}
	})
}

func TestProbeBatch(t *testing.T) {
	ctx := context.Background()

	t.Run("parses queue column", func(t *testing.T) {
		b := &Backend{
			Cfg: &hpcconfig.Config{StatusListTemplate: "squeue"},
			Run: func(ctx context.Context, cmd string) (string, error) {
				return "12345 RUNNING gpu-a100\n67890 PENDING cpu-batch\n", nil
			},
		}
		result, err := b.probeBatch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 2 {
			t.Fatalf("got %d entries, want 2", len(result))
		}
		r1 := result["12345"]
		if r1.Signal != SchedRunning || r1.NativeState != "RUNNING" || r1.Queue != "gpu-a100" {
			t.Fatalf("unexpected result for 12345: %+v", r1)
		}
		r2 := result["67890"]
		if r2.Signal != SchedPending || r2.NativeState != "PENDING" || r2.Queue != "cpu-batch" {
			t.Fatalf("unexpected result for 67890: %+v", r2)
		}
	})

	t.Run("two columns no queue", func(t *testing.T) {
		b := &Backend{
			Cfg: &hpcconfig.Config{StatusListTemplate: "squeue"},
			Run: func(ctx context.Context, cmd string) (string, error) {
				return "12345 RUNNING\n", nil
			},
		}
		result, err := b.probeBatch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		r := result["12345"]
		if r.Queue != "" {
			t.Fatalf("expected empty queue, got %q", r.Queue)
		}
	})

	t.Run("signal_map applied in batch", func(t *testing.T) {
		b := &Backend{
			Cfg: &hpcconfig.Config{
				StatusListTemplate: "squeue",
				SignalMap:           map[string]string{"COMPLETING": "running"},
			},
			Run: func(ctx context.Context, cmd string) (string, error) {
				return "12345 COMPLETING gpu\n", nil
			},
		}
		result, err := b.probeBatch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		r := result["12345"]
		if r.Signal != SchedRunning {
			t.Fatalf("got signal %q, want running", r.Signal)
		}
		if r.NativeState != "COMPLETING" {
			t.Fatalf("got native %q, want COMPLETING", r.NativeState)
		}
	})
}
