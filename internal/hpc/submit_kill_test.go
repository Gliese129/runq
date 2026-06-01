package hpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/hpccore"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
)

func skipIfNoHPCCore(t *testing.T) {
	t.Helper()
	if _, err := hpccore.Render("{{x}}", map[string]string{"x": "y"}); errors.Is(err, hpccore.ErrNotImplemented) {
		t.Skip("internal/hpccore not implemented yet")
	}
}

func newTestBackend(t *testing.T, cfg *hpcconfig.Config, runner Runner) (*Backend, *project.Config, job.JobConfig) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())
	proj := &project.Config{ProjectName: "p", WorkingDir: t.TempDir(), CmdTemplate: "python x.py --lr {{lr}}"}
	jobCfg := job.JobConfig{
		Project: "p",
		Sweep:   []job.SweepBlock{{Method: "grid", Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1}}}}},
	}
	return &Backend{Cfg: cfg, Store: st, Run: runner}, proj, jobCfg
}

// #1: a submit that succeeds on the cluster but whose id can't be parsed must
// leave a VISIBLE task (the durable ledger row), not an invisible orphan. It
// stays pending — NOT failed — because the job may be running untracked, and
// pending is non-terminal so Refresh won't stamp a bogus finished_at.
func TestSubmitUnparseableIDLeavesVisibleTask(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
	runner := func(ctx context.Context, command string) (string, error) {
		if strings.HasPrefix(command, "submit") {
			return "GARBAGE no id here", nil // sbatch "succeeded" but no parseable id
		}
		return "", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	ctx := context.Background()

	jobID, submitted, err := b.Submit(ctx, jobCfg, proj, SubmitOpts{SkipPreflight: true})
	if err == nil {
		t.Fatal("expected an error when the submit id is unparseable")
	}
	if submitted != 0 {
		t.Errorf("submitted = %d, want 0", submitted)
	}
	tasks, _ := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if len(tasks) != 1 {
		t.Fatalf("want 1 visible ledger task, got %d", len(tasks))
	}
	if tasks[0].Status != "pending" {
		t.Errorf("task status = %s, want pending (not a failure — job may be running untracked)", tasks[0].Status)
	}
	if tasks[0].FinishedAt != nil {
		t.Errorf("finished_at must not be set on an id-loss task")
	}
}

// #3: when the cancel command fails, the task must NOT be marked killed (else
// the DB says killed while the cluster job keeps running).
func TestKillFailureLeavesTaskUnkilled(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
	runner := func(ctx context.Context, command string) (string, error) {
		switch {
		case strings.HasPrefix(command, "submit"):
			return "job 7", nil
		case strings.HasPrefix(command, "cancel"):
			return "scancel: connection error", fmt.Errorf("exit status 1")
		}
		return "", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	ctx := context.Background()

	jobID, _, err := b.Submit(ctx, jobCfg, proj, SubmitOpts{SkipPreflight: true})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	tasks, _ := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if _, err := b.Kill(ctx, tasks[0].ID); err == nil {
		t.Fatal("expected Kill to error when the cancel command fails")
	}
	got, _ := b.Store.GetTask(ctx, tasks[0].ID)
	if got.Status == "killed" {
		t.Errorf("task marked killed despite cancel failure (state divergence)")
	}
}

// #3 (no-id case): an id-loss task (pending, empty external_id) cannot be
// cancelled — Kill must refuse and NOT mark it killed (the job may be running
// untracked; recording killed would be a lie about what runq did).
func TestKillNoExternalIDRefuses(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
	runner := func(ctx context.Context, command string) (string, error) {
		if strings.HasPrefix(command, "submit") {
			return "GARBAGE", nil // id unparseable → task left pending, no external_id
		}
		return "", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	ctx := context.Background()

	jobID, _, _ := b.Submit(ctx, jobCfg, proj, SubmitOpts{SkipPreflight: true}) // returns error; the task is left pending
	tasks, _ := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if len(tasks) != 1 || tasks[0].ExternalID != "" {
		t.Fatalf("precondition: want 1 task with empty external_id, got %+v", tasks)
	}

	killed, err := b.Kill(ctx, tasks[0].ID)
	if err == nil {
		t.Fatal("expected Kill to refuse a task with no external id")
	}
	if killed != 0 {
		t.Errorf("killed = %d, want 0", killed)
	}
	got, _ := b.Store.GetTask(ctx, tasks[0].ID)
	if got.Status == "killed" {
		t.Errorf("task marked killed despite no cancel performed")
	}
}
