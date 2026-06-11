package hpc

import (
	"os/exec"
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
	"github.com/gliese129/runq/internal/submitplan"
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

// F1: task params are exposed to submit_template as {{param.*}}, so per-task
// scheduler knobs (walltime, queue, job name) can live in the sweep.
func TestSubmitTemplateParamNamespace(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{
		SubmitTemplate: "qsub -l h_rt={{param.h_rt}} -N {{param.lr}} {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
	}
	var captured string
	runner := func(ctx context.Context, command string) (string, error) {
		if strings.HasPrefix(command, "qsub") {
			captured = command
			return "job 42", nil
		}
		return "", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	proj.CmdTemplate = "python x.py --lr {{lr}} --h-rt {{h_rt}}"
	jobCfg.FixedParams = map[string]any{"h_rt": "4:00:00"}

	if _, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !strings.Contains(captured, "-l h_rt='4:00:00'") {
		t.Errorf("fixed param not rendered into submit command: %q", captured)
	}
	if !strings.Contains(captured, "-N '0.1'") {
		t.Errorf("swept param not rendered into submit command: %q", captured)
	}
}

// F2: a failing setup_command aborts before anything is persisted/submitted.
func TestSetupCommandFailureAborts(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
	submitCalls := 0
	runner := func(ctx context.Context, command string) (string, error) {
		submitCalls++
		return "job 1", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	proj.SetupCommand = "exit 7"

	_, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true})
	if err == nil {
		t.Fatal("failing setup must abort the submit")
	}
	if submitCalls != 0 {
		t.Errorf("cluster submit ran despite setup failure (%d calls)", submitCalls)
	}
	jobs, _ := b.Store.ListJobs(context.Background(), "")
	if len(jobs) != 0 {
		t.Errorf("job row persisted despite setup failure: %d rows", len(jobs))
	}
}

// Scheduler-only params ({{param.*}} in submit_template) must not be
// demanded by command_template nor leak into {{args}} — no fake-consumption
// workarounds needed.
func TestSchedulerParamsExemptFromCommand(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{
		SubmitTemplate: "qsub -l h_rt={{param.h_rt}} {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
	}
	var captured string
	runner := func(ctx context.Context, command string) (string, error) {
		if strings.Contains(command, "qsub") {
			captured = command
			return "job 7", nil
		}
		return "", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	// command_template does NOT consume h_rt — and has no {{args}} either.
	proj.CmdTemplate = "python x.py --lr {{lr}}"
	jobCfg.FixedParams = map[string]any{"h_rt": "8:00:00"}

	if _, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true}); err != nil {
		t.Fatalf("scheduler param must not require command consumption: %v", err)
	}
	if !strings.Contains(captured, "-l h_rt='8:00:00'") {
		t.Errorf("h_rt missing from submit command: %q", captured)
	}
}

// Regression: the env prefix must use `export K=v; cmd` — the `K=v cmd $K`
// form expands $K BEFORE the assignment takes effect (POSIX), so a
// submit_template referencing $TSUBAME_GROUP would see the old/empty value.
func TestSubmitEnvPrefixResolvesSameLineReferences(t *testing.T) {
	prefix := submitEnvPrefix(map[string]string{"TSUBAME_GROUP": "tga-demo", "B": "2"})
	out, err := exec.Command("sh", "-c", prefix+`printf '%s/%s' "$TSUBAME_GROUP" "$B"`).CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v (%s)", err, out)
	}
	if string(out) != "tga-demo/2" {
		t.Fatalf("same-line $VAR did not resolve from prefix: got %q", out)
	}
}

// End-to-end pin for the {{name}} chain: project.job_name (default template)
// → job.Name (per-submit override) → render+sanitize → {{name}} in the
// ACTUAL submit command the scheduler receives. Plan-level coverage exists
// (TestPlannedTaskName); this asserts the last hop — renderSubmitCmd's vars
// table actually carries the name, and the override beats the project
// template.
func TestSubmitCommandReceivesRenderedJobName(t *testing.T) {
	skipIfNoHPCCore(t)
	cfg := &hpcconfig.Config{
		SubmitTemplate: "qsub -N {{name}} {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
	}
	var captured []string
	runner := func(ctx context.Context, command string) (string, error) {
		if strings.Contains(command, "qsub") {
			captured = append(captured, command)
			return "job 42", nil
		}
		return "", nil
	}

	// 1. project.job_name template (params + sanitization: "/" → "-")
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	proj.JobName = "eval/{{lr}}"
	if _, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !strings.Contains(captured[0], "-N 'eval-0.1'") {
		t.Errorf("project job_name not rendered into -N: %q", captured[0])
	}

	// 2. job.yaml name: override wins over the project template
	captured = nil
	jobCfg.Name = "override-{{lr}}"
	if _, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true}); err != nil {
		t.Fatalf("submit with override: %v", err)
	}
	if !strings.Contains(captured[0], "-N 'override-0.1'") {
		t.Errorf("job name override not rendered into -N: %q", captured[0])
	}
}

// run.sh contract (from a real TSUBAME failure): schedulers start jobs in
// $HOME, so without a cd the command's relative paths break (exit 127); and
// without the exec redirect the DB's log_path never receives a byte (output
// only went wherever -o/-e pointed).
func TestRunScriptCdsAndRedirects(t *testing.T) {
	b := &Backend{Cfg: &hpcconfig.Config{}}
	script := b.buildRunScript(submitplan.PlannedTask{
		TaskID: "tk1", TaskDir: "/ws/jb1/tk1", LogPath: "/ws/jb1/tk1.log",
		WorkingDir: "/home/u/proj", Command: "bash scripts/run.sh",
	}, submitplan.Plan{JobID: "jb1", Project: "p"})

	if !strings.Contains(script, "exec >> '/ws/jb1/tk1.log' 2>&1") {
		t.Errorf("missing log redirect:\n%s", script)
	}
	if !strings.Contains(script, "if cd '/home/u/proj'; then") {
		t.Errorf("missing cd into working_dir:\n%s", script)
	}
	// cd must come BEFORE the command, redirect before everything that prints
	cdPos := strings.Index(script, "if cd ")
	cmdPos := strings.Index(script, "bash scripts/run.sh")
	execPos := strings.Index(script, "exec >>")
	statusPos := strings.Index(script, "_runq_status \"$(printf '{\"status\":\"running\"")
	if !(execPos < statusPos && cdPos < cmdPos) {
		t.Errorf("order wrong (exec=%d status=%d cd=%d cmd=%d):\n%s", execPos, statusPos, cdPos, cmdPos, script)
	}
	// failed cd must yield a failed status, not a silent half-run
	if !strings.Contains(script, "code=1") {
		t.Errorf("cd failure must set a nonzero exit code:\n%s", script)
	}
}
