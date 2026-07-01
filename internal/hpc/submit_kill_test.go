package hpc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/submitplan"
)

func newTestBackend(t *testing.T, cfg *config.TargetConfig, run func(ctx context.Context, command string) (string, error)) (*Backend, *project.Config, job.JobConfig) {
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
	return &Backend{Cfg: cfg, Store: st, FS: newTestFSFromRunner(run)}, proj, jobCfg
}

// #1: a submit that succeeds on the cluster but whose id can't be parsed must
// leave a VISIBLE task (the durable ledger row), not an invisible orphan. It
// stays pending — NOT failed — because the job may be running untracked, and
// pending is non-terminal so reconcile won't stamp a bogus finished_at.
func TestSubmitUnparseableIDLeavesVisibleTask(t *testing.T) {

	cfg := &config.TargetConfig{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
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

	cfg := &config.TargetConfig{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
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

	cfg := &config.TargetConfig{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
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

	cfg := &config.TargetConfig{
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

	cfg := &config.TargetConfig{SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, KillTemplate: "cancel {{ext_id}}"}
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

	cfg := &config.TargetConfig{
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

	cfg := &config.TargetConfig{
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
	b := &Backend{Cfg: &config.TargetConfig{}}
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
	// last-words trap: TERM (qdel/walltime grace) must be caught; USR1/USR2
	// must NOT be (frameworks use them for checkpoint signaling)
	if !strings.Contains(script, "TERM INT HUP") {
		t.Errorf("missing kill trap:\n%s", script)
	}
	if strings.Contains(script, "USR1") || strings.Contains(script, "USR2") {
		t.Errorf("run.sh must not intercept USR1/USR2:\n%s", script)
	}
	// the whole script must be valid POSIX shell (quoting in the trap line
	// is easy to get wrong) — sh -n parses without executing
	f := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
		t.Errorf("generated run.sh is not valid shell: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

// End-to-end trap behavior: run a real (non-scheduler) shell, TERM the
// process group mid-run, and assert the wrapper wrote the killed status.
func TestRunScriptTrapWritesKilledOnTerm(t *testing.T) {
	dir := t.TempDir()
	b := &Backend{Cfg: &config.TargetConfig{}}
	script := b.buildRunScript(submitplan.PlannedTask{
		TaskID: "tk1", TaskDir: dir, LogPath: filepath.Join(dir, "tk1.log"),
		WorkingDir: dir, Command: "sleep 30",
	}, submitplan.Plan{JobID: "jb1", Project: "p"})
	f := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", f)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// wait until the wrapper reported running (trap installed before that)
	statusPath := filepath.Join(dir, "status.json")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if buf, err := os.ReadFile(statusPath); err == nil && strings.Contains(string(buf), "running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wrapper never reported running")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// scheduler-style kill: signal the process GROUP (sh + sleep)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	_ = cmd.Wait()
	buf, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("status.json unreadable after TERM: %v", err)
	}
	if !strings.Contains(string(buf), `"status":"killed"`) {
		t.Errorf("trap did not write killed status, got: %s", buf)
	}
}

// TTL-based throttle: repeated EnsureFresh calls within the TTL window skip
// the scheduler probe but still run local reconcile (status.json + metrics).
// A zero TTL always forces a full reconcile including the probe.
func TestEnsureFreshTTLThrottle(t *testing.T) {

	cfg := &config.TargetConfig{
		SubmitTemplate: "qsub {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
		StatusTemplate: "checkstat {{ext_id}}",
	}
	probes := 0
	runner := func(ctx context.Context, command string) (string, error) {
		if strings.Contains(command, "qsub") {
			return "job 42", nil
		}
		if strings.Contains(command, "checkstat") {
			probes++
			return "RUNNING", nil
		}
		return "", nil
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	jobID, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Three calls with a long TTL: only the first should probe the scheduler.
	for i := 0; i < 3; i++ {
		if err := b.EnsureFresh(context.Background(), jobID, 30*time.Second); err != nil {
			t.Fatalf("EnsureFresh #%d: %v", i, err)
		}
	}
	if probes != 1 {
		t.Errorf("3 EnsureFresh calls within TTL should probe exactly once, got %d", probes)
	}

	// Explicit TTL=0 = force reconcile → always probes (task still non-terminal).
	if err := b.EnsureFresh(context.Background(), jobID, 0); err != nil {
		t.Fatalf("EnsureFresh(0): %v", err)
	}
	if probes != 2 {
		t.Errorf("EnsureFresh(0) must bypass the TTL cache, got %d probes", probes)
	}

	// Even within TTL, local reconcile still runs: a wrapper status.json
	// written after the first call must be picked up by the next call.
	tasks, _ := b.Store.ListTasks(context.Background(), store.TaskFilter{JobID: jobID})
	tk := tasks[0]
	writeFile(t, filepath.Join(tk.TaskDir, statusFileName),
		`{"status":"success","exit_code":0,"finished_at":1730000000}`)

	probesBefore := probes
	if err := b.EnsureFresh(context.Background(), jobID, 30*time.Second); err != nil {
		t.Fatalf("EnsureFresh(within TTL after wrapper write): %v", err)
	}
	if probes != probesBefore {
		t.Errorf("within-TTL call should NOT probe again, but probes went %d→%d", probesBefore, probes)
	}
	got, _ := b.Store.GetTask(context.Background(), tk.ID)
	if got.Status != "success" {
		t.Errorf("within-TTL call must still pick up wrapper status.json: got %s, want success", got.Status)
	}
}

// Listing-style status templates (no {{ext_id}} — SGE/UGE presets run a full
// `qstat` and row-select in the parser) must cost ONE scheduler query per
// refresh pass regardless of task count.
func TestListingStatusTemplateBatchesPerPass(t *testing.T) {

	cfg := &config.TargetConfig{
		SubmitTemplate: "qsub {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
		StatusTemplate: "qstat-all", // listing-style: identical for every task
		StatusParser:   []string{`awk -v id={{ext_id}} '$1==id{print $2; f=1} END{if(!f) print "gone"}'`},
	}
	statCalls, submits := 0, 0
	runner := func(ctx context.Context, command string) (string, error) {
		switch {
		case strings.HasPrefix(command, "qsub"):
			submits++
			return fmt.Sprintf("job %d", 100+submits), nil
		case command == "qstat-all":
			statCalls++
			return "101 RUNNING\n102 RUNNING", nil
		default: // parser pipelines etc. — local shell, run for real
			out, err := exec.Command("sh", "-c", command).CombinedOutput()
			return string(out), err
		}
	}
	b, proj, jobCfg := newTestBackend(t, cfg, runner)
	proj.CmdTemplate = "python x.py --lr {{lr}}"
	jobCfg.Sweep = []job.SweepBlock{{Method: "grid",
		Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1, 0.2}}}}}

	jobID, n, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true})
	if err != nil || n != 2 {
		t.Fatalf("submit: n=%d err=%v", n, err)
	}
	if err := b.EnsureFresh(context.Background(), jobID, 0); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if statCalls != 1 {
		t.Errorf("2 tasks with a listing-style template should run the status command ONCE per pass, got %d", statCalls)
	}
	// and the per-task row selection still works: both tasks now running
	tasks, _ := b.Store.ListTasks(context.Background(), store.TaskFilter{JobID: jobID})
	for _, tk := range tasks {
		if tk.Status != "running" {
			t.Errorf("task %s (ext %s): want running, got %s", tk.ID, tk.ExternalID, tk.Status)
		}
	}
}

// New work must not land in an archived project — the cascade hides its
// jobs from the default lists, so the run would be invisible. The guard
// lives in Submit (one backend point: GUI rerun/draft/import, CLI,
// --project-file all pass through here).
func TestSubmitRefusesArchivedProject(t *testing.T) {

	cfg := &config.TargetConfig{
		SubmitTemplate: "qsub {{run_sh}}",
		SubmitIDRegex:  `job ([0-9]+)`,
		KillTemplate:   "cancel {{ext_id}}",
	}
	runner := func(ctx context.Context, command string) (string, error) { return "job 1", nil }
	b, proj, jobCfg := newTestBackend(t, cfg, runner)

	// First submit registers the project; archive it, then try again.
	if _, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	reg := project.NewRegistry(b.Store.DB())
	// jobs from the first submit are pending — flip them terminal so the
	// project archive guard lets us archive.
	if _, err := b.Store.DB().Exec(`UPDATE jobs SET status = 'done'`); err != nil {
		t.Fatal(err)
	}
	if err := reg.Archive(context.Background(), proj.ProjectName); err != nil {
		t.Fatalf("archive project: %v", err)
	}
	if _, _, err := b.Submit(context.Background(), jobCfg, proj, SubmitOpts{SkipPreflight: true}); err == nil {
		t.Fatal("submit into an archived project must be refused")
	} else if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("error should explain the archive state, got: %v", err)
	}
}
