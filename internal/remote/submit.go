package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/preflight"
	"github.com/gliese129/runq-lab/internal/project"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/submitplan"
	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/gliese129/runq-lab/internal/workspace"
)

// SubmitOpts carries per-call submit options, mirroring service.SubmitJobOpts on
// the daemon side so the two backends stay symmetric.
type SubmitOpts struct {
	// SkipPreflight disables the fail-before-submit checks (pip/import/path).
	// On HPC these run on the login node, which may not match the compute node
	// environment, so a skip flag is useful (CLI: --no-preflight).
	SkipPreflight bool
}

// Submit compiles the job into a Plan (shared kernel), then for each task:
// writes the workspace, generates run.sh, runs the submit template, extracts
// the external id, and persists the task row. The proj must already be resolved
// by the caller (from the HPC registry or a project file).
//
// Persistence order: job row first, then each task row after a successful
// submit. Unlike the daemon there is no atomic all-or-nothing — sbatch is an
// external side effect, so a mid-batch failure leaves the already-submitted
// tasks recorded and returns how many made it.
// renderSubmitCmd renders the submit template for one task. Task params are
// facts of the task — exposed as {{param.*}} so per-task scheduler knobs
// (walltime, queue, job name) can live in the sweep.
func renderSubmitCmd(tmpl string, t submitplan.PlannedTask, plan submitplan.Plan, runsh string) (string, error) {
	vars := map[string]string{
		"run_sh":   runsh,
		"gpus":     strconv.Itoa(t.GPUsNeeded),
		"timeout":  strconv.Itoa(t.Timeout),
		"job_id":   plan.JobID,
		"task_id":  t.TaskID,
		"task_dir": t.TaskDir,
		"name":     t.Name,
		"project":  plan.Project,
		"log_path": t.LogPath,
	}
	for name, value := range t.Params {
		vars["param."+name] = fmt.Sprintf("%v", value)
	}
	return utils.Render(tmpl, vars)
}

// envPrelude assembles this lane's execution environment through THE
// single injection point (utils.EnvPrelude, Codex r2 #2): env_setup, the
// resolved HOME, the ambient .env and the merged env. run.sh, the
// preflight probe, setup_command and the SUBMIT COMMAND all render
// through this — a cluster whose qsub/sbatch comes from `module load`
// (env_setup) gets it on every surface, and export-then-command ordering
// means $TSUBAME_GROUP-style references in submit_template resolve from
// project config, not from whatever the login shell happens to export.
func (b *Backend) envPrelude(env map[string]string) utils.EnvPrelude {
	home, _ := b.homeDirCached() // "" on failure: prelude omits the export
	return utils.EnvPrelude{
		Home:     home,
		EnvSetup: b.Cfg.EnvSetup,
		EnvFile:  env["RUNQ_ENV_FILE"],
		Env:      env,
	}
}

// printPreflight prints the three-state report (one line per check) so
// skipped checks stay visible — "didn't check" must look different from
// "checked and passed".
func printPreflight(r preflight.Report) {
	for _, c := range r.Results {
		mark := "-"
		switch c.Status {
		case "passed":
			mark = "✓"
		case "failed":
			mark = "✗"
		case "warning":
			mark = "!"
		}
		fmt.Printf("%s %-10s %s\n", mark, c.Name, c.Detail)
		// Remediation commands (e.g. HF pre-download): copy-paste ready.
		for _, cmd := range c.Commands {
			fmt.Printf("             $ %s\n", cmd)
		}
	}
}

// resolveNote renders note placeholders against this project's existing
// job notes ({{version}} family scan needs them).
func resolveNote(ctx context.Context, st *store.Store, cfg job.JobConfig) (string, error) {
	rows, err := st.ListJobs(ctx, cfg.Project, "")
	if err != nil {
		return "", fmt.Errorf("list jobs for note rendering: %w", err)
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		notes = append(notes, r.Note)
	}
	return job.RenderNote(&cfg, job.NoteContext{
		Project: cfg.Project, Now: time.Now(), ExistingNotes: notes,
	})
}

// Prepare does everything up to (but NOT including) the external submission:
// plan, validate, one-shot setup, workspace files, run.sh, submit.cmd, then
// one atomic job+tasks ledger transaction. The returned rows are ready
// to be queued; the actual submission happens per task in Launcher.Launch.
//
// planDeps assembles the submitplan.Deps fields that Prepare and Preview
// MUST agree on (RQ-65 retest #1: preview forgot PreflightFS and checked
// TSUBAME paths on the Mac). Callers add only their true differences:
// JobID and Paths (real root vs placeholder).
//
// Checks run against the TARGET: paths/script via its FS, probes on its
// login node. The scope label is the honest caveat that the login-node
// env may still differ from compute nodes.
func (b *Backend) planDeps(skipPreflight bool) submitplan.Deps {
	return submitplan.Deps{
		IDGen:                 utils.GenerateTaskID,
		SkipPreflight:         skipPreflight,
		PreflightDisableLocal: b.Cfg.PreflightLocal != nil && !*b.Cfg.PreflightLocal,
		PreflightScope:        fmt.Sprintf("on %s login node", b.Cfg.Name),
		PreflightFS:           b.FS,
		PreflightEnvSetup:     b.Cfg.EnvSetup,
		SchedulerParams:       config.HPCTemplateParamRefs(b.Cfg.SubmitTemplate),
	}
}

// WorkspaceRoot is THE single decision point for where this target puts
// job workspaces (RQ-65): a configured target workspace WINS — it is a
// path on the TARGET's filesystem, composed as POSIX and materialized via
// b.FS. The global data_path/working_dir resolution is the on-target /
// legacy fallback only. Prepare (real submit), PreviewSubmit and dry-run
// ALL call this — preview computing its own root is how dry-run ends up
// confirming a path the submit never uses.
//
// materialize=false is the read-only twin (preview): same branch, no mkdir.
func (b *Backend) WorkspaceRoot(proj *project.Config, materialize bool) (string, error) {
	if b.Cfg.Workspace != "" {
		return b.Cfg.Workspace, nil
	}
	if materialize {
		return config.ResolveRoot(b.StorageCfg, proj.WorkingDir, proj.ProjectName)
	}
	return config.ProspectiveRoot(b.StorageCfg, proj.WorkingDir, proj.ProjectName), nil
}
func (b *Backend) Prepare(ctx context.Context, jobCfg job.JobConfig, proj *project.Config, opts SubmitOpts) (jobID string, rows []store.TaskRow, err error) {
	// Satisfy the jobs.project_name foreign key: ensure the project exists in
	// the HPC store. Add-if-missing covers both "resolved from registry" and
	// "loaded from a file" callers.
	reg := project.NewRegistry(b.Store.DB()).WithFSRouter(func(target string) rfs.FS {
		if target == b.Cfg.Name {
			return b.FS
		}
		return nil // not ours: local fallback (fault-tolerant)
	})
	if _, gerr := reg.Get(ctx, proj.ProjectName); gerr != nil {
		if aerr := reg.Add(ctx, *proj); aerr != nil {
			return "", nil, fmt.Errorf("register project %q: %w", proj.ProjectName, aerr)
		}
	}
	// THE submit precondition (exists + not archived, fail-closed) lives in
	// one place — see Registry.RequireSubmittable.
	if err := reg.RequireSubmittable(ctx, proj.ProjectName); err != nil {
		return "", nil, err
	}

	jobID = utils.GenerateJobID()
	wsRoot, err := b.WorkspaceRoot(proj, true)
	if err != nil {
		return "", nil, fmt.Errorf("resolve workspace root: %w", err)
	}

	// Resolved note for display (job row, workspace files); ConfigJSON keeps
	// the template so re-submission keeps incrementing {{version}}.
	planCfg := jobCfg
	if resolved, nerr := resolveNote(ctx, b.Store, jobCfg); nerr != nil {
		return "", nil, nerr
	} else {
		planCfg.Note = resolved
	}
	// Job dir carries the resolved note so `ls .runq/` reads like an
	// experiment log; the DB stores full paths (never re-derived from id).
	jobRoot := path.Join(wsRoot, workspace.JobDirName(planCfg.Note, jobID))

	deps := b.planDeps(opts.SkipPreflight)
	deps.JobID = jobID
	deps.Paths = submitplan.Paths{WorkspaceRoot: jobRoot, LogRoot: jobRoot}
	plan, err := submitplan.Build(ctx, planCfg, proj, deps)
	if err != nil {
		return "", nil, err
	}
	printPreflight(plan.Preflight)

	// A plan with zero tasks means the sweep expanded to nothing — a config
	// mistake, surfaced as a validation error before anything persists.
	if len(plan.Tasks) == 0 {
		return "", nil, fmt.Errorf("sweep expands to zero tasks — check sweep parameters (nothing would be submitted)")
	}

	// Fail-fast template validation: render the submit command for the
	// first task BEFORE anything is persisted or submitted. A missing
	// {{param.*}} must abort with zero residue and a fix-it hint, not die
	// mid-loop with the job row already inserted.
	if len(plan.Tasks) > 0 {
		if _, err := renderSubmitCmd(b.Cfg.SubmitTemplate, plan.Tasks[0], plan, "<run_sh>"); err != nil {
			return "", nil, fmt.Errorf(
				"%w\nHint: every {{param.NAME}} in submit_template must exist as a task param — add it to fixed_params (same value for all tasks) or as a sweep column (per-task values). Try `runq hpc submit --dry-run` to preview",
				err)
		}
	}

	// One-shot job setup (e.g. model pre-download on the login node).
	// Runs before any DB row or cluster submission — failure leaves nothing.
	// The prelude is the SAME environment preflight probed and run.sh will
	// export (env_setup + .env + merged env + HOME) — Codex r2 #3.
	if err := submitplan.RunSetup(ctx, proj, jobCfg, b.FS, b.envPrelude(plan.Env)); err != nil {
		return "", nil, err
	}

	now := time.Now()
	cfgJSON, err := json.Marshal(jobCfg)
	if err != nil {
		return "", nil, fmt.Errorf("marshal job config: %w", err)
	}
	jobRow := store.JobRow{
		ID: plan.JobID, ProjectName: plan.Project, Description: plan.Description,
		Note: plan.Note, ConfigJSON: string(cfgJSON), Status: "pending",
		TotalTasks: len(plan.Tasks), Target: b.Cfg.Name, CreatedAt: now,
	}

	for _, t := range plan.Tasks {
		// Local prep + template render first: these have no external side effect,
		// so a failure here aborts before any cluster job or DB row exists.
		if err := workspace.WriteFS(b.FS, t.TaskDir, t.Params, plan.Wandb, plan.Note); err != nil {
			return plan.JobID, rows, fmt.Errorf("prepare workspace for %s: %w", t.TaskID, err)
		}
		runsh := path.Join(t.TaskDir, runScriptName)
		if err := b.FS.WriteFile(runsh, []byte(b.buildRunScript(t, plan)), 0o755); err != nil {
			return plan.JobID, rows, fmt.Errorf("write run.sh for %s: %w", t.TaskID, err)
		}
		cmd, err := renderSubmitCmd(b.Cfg.SubmitTemplate, t, plan, runsh)
		if err != nil {
			return plan.JobID, rows, fmt.Errorf("render submit_template for %s: %w", t.TaskID, err)
		}
		fullCmd := b.envPrelude(plan.Env).Render() + cmd

		// Persist the rendered command next to run.sh. The scheduler-driven
		// launch path (remote.Launcher) replays this file verbatim, so retries
		// re-run exactly what was validated here. 0o600: may embed env values.
		if err := b.FS.WriteFile(path.Join(t.TaskDir, submitCmdFileName), []byte(fullCmd), 0o600); err != nil {
			return plan.JobID, rows, fmt.Errorf("write submit.cmd for %s: %w", t.TaskID, err)
		}

		// Build the complete ledger image in memory. No external submission can
		// happen until the job and every pending task commit together below.
		row := planToTaskRow(t, plan, now, "", b.Cfg.Name, b.Cfg.SemanticGeneration())
		rows = append(rows, row)
	}

	// Admission is one publication boundary: callers may enqueue only after
	// every task exists durably. A task insert or commit failure rolls back the
	// job row and all preceding task rows, satisfying the A1 DFA invariant.
	if err := b.Store.InsertJobWithTasks(ctx, &jobRow, rows); err != nil {
		return plan.JobID, nil, fmt.Errorf("persist job and tasks atomically: %w", err)
	}

	return plan.JobID, rows, nil
}

// homeDirCached resolves the login node's absolute $HOME once per lane
// (RQ-76 ①). Resolution happens where sshd sets HOME correctly; the value
// is then baked into run.sh so the COMPUTE node — whose scheduler may
// strip the environment (--export=NONE) — still expands `~` right.
// Failure is remembered and non-fatal: run.sh simply skips the export
// (most schedulers do pass HOME; restoration is belt and suspenders).
func (b *Backend) homeDirCached() (string, error) {
	b.homeOnce.Do(func() {
		fsys := b.FS
		if fsys == nil {
			fsys = rfs.NewLocalFS() // tests / legacy local lanes
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stdout, stderr, code, err := fsys.Exec(ctx, "sh", "-c", `printf %s "$HOME"`)
		if err != nil {
			b.homeErr = err
			return
		}
		if code != 0 {
			b.homeErr = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(string(stderr)))
			return
		}
		home := strings.TrimSpace(string(stdout))
		if !strings.HasPrefix(home, "/") {
			b.homeErr = fmt.Errorf("resolved HOME %q is not absolute", home)
			return
		}
		b.homeDir = home
	})
	return b.homeDir, b.homeErr
}

// buildRunScript assembles the wrapper script. The script SKELETON is glue; the
// security-sensitive part — escaping every env value — is delegated to
// utils.ShellQuote. The user's command (t.Command, already env-activation
// wrapped by the kernel) is embedded verbatim, exactly as the daemon would exec
// it.
func (b *Backend) buildRunScript(t submitplan.PlannedTask, plan submitplan.Plan) string {
	env := make(map[string]string, len(t.Env)+10)
	for k, v := range t.Env {
		env[k] = v
	}
	// Shared RUNQ_* contract; HPC adds RUNQ_NO_DAEMON and sets no socket.
	// Safety uses the same defaults as the daemon scheduler (110%, 0 GB).
	// Even without freeze, the SDK's pre-flight disk check catches low-disk
	// early and raises RunqDiskFullError instead of letting the save hit
	// ENOSPC mid-write.
	for k, v := range workspace.BaseEnv(workspace.Identity{
		TaskID: t.TaskID, JobID: plan.JobID, Project: plan.Project, TaskDir: t.TaskDir,
		SweepKeys: plan.SweepKeys, JobNote: plan.Note,
	}, workspace.Safety{FactorPercent: 110}) {
		env[k] = v
	}
	// A runqd-backed target injects RUNQ_SOCKET_PATH into the child process, so
	// its SDK calls should use that machine daemon. Traditional HPC schedulers
	// have no daemon on the compute node and retain the explicit no-daemon mode.
	if b.Cfg == nil || b.Cfg.Scheduler != "runq" {
		env["RUNQ_NO_DAEMON"] = "1"
	}
	// Done-marker dir: enables O(1) completion detection (one readdir instead
	// of N status.json reads). Only baked in when the target has a workspace
	// root; without it the daemon falls back to probe-only reconcile.
	if dir := b.doneDir(); dir != "" {
		env["RUNQ_DONE_DIR"] = dir
	}
	// runq binary on the target (absolute — batch environments have minimal
	// PATH): enables compute-node work at task end, currently the metrics
	// pyramid build. Absent = the step is skipped, everything else works.
	if b.Cfg.RunqBin != "" {
		env["RUNQ_BIN"] = b.Cfg.RunqBin
	}

	var s strings.Builder
	s.WriteString("#!/bin/sh\n")
	s.WriteString("# Generated by runq hpc. Do not edit.\n")
	// Ambient env file: sourced FIRST, so the explicit exports below win
	// (precedence: .env < project/override env). Values are resolved on the
	// compute node at start time — never copied into this script.
	// runq owns log placement: all task output goes to the DB's log_path,
	// so `runq logs` / the dashboard work regardless of how the user's
	// submit_template routes -o/-e (those keep only scheduler-level noise).
	fmt.Fprintf(&s, "exec >> %s 2>&1\n", utils.ShellQuote(t.LogPath))
	// THE environment (utils.EnvPrelude — same injection point as the
	// preflight probe, setup_command and the submit command): HOME
	// restored from the login node (RQ-76 ①: batch shells may strip it;
	// with an absolute HOME exported first every `~` in env_setup / user
	// code expands natively), then env_setup, then the ambient .env, then
	// the explicit exports which must win. Placed after the log redirect
	// so env_setup output lands in the task log.
	s.WriteString(b.envPrelude(env).Render())
	fmt.Fprintf(&s, "STATUS_FILE=%s\n", utils.ShellQuote(path.Join(t.TaskDir, statusFileName)))
	s.WriteString(`_runq_status() { printf '%s\n' "$1" > "$STATUS_FILE.tmp" && mv "$STATUS_FILE.tmp" "$STATUS_FILE"; }` + "\n")
	// Done marker: written AFTER the terminal status.json, so a marker's
	// presence guarantees the status file is readable. The daemon's marker
	// scan turns one readdir into "which tasks finished since last look".
	s.WriteString(`_runq_done() { if [ -n "$RUNQ_DONE_DIR" ]; then mkdir -p "$RUNQ_DONE_DIR" 2>/dev/null; : > "$RUNQ_DONE_DIR/$RUNQ_TASK_ID"; fi; }` + "\n")
	// Last words: schedulers send TERM (qdel/scancel/walltime) with a grace
	// period before KILL — write a terminal status while we still can, so
	// the kill becomes a wrapper FACT instead of a qstat inference.
	// Deliberately NOT trapping USR1/USR2: training frameworks use those
	// for checkpoint signaling and runq must not intercept them.
	s.WriteString(`trap 'kill $_RUNQ_ACTIVITY_PID 2>/dev/null; _runq_status "$(printf '\''{"status":"killed","exit_code":143,"finished_at":%s}'\'' "$(date +%s)")"; _runq_done; exit 143' TERM INT HUP` + "\n")
	s.WriteString(`_runq_status "$(printf '{"status":"running","started_at":%s}' "$(date +%s)")"` + "\n")
	// Activity sidecar: sample log file size + incremental line count every 60s
	// into 3-column activity.tsv {ts, bytes, lines}. The trap kills the sidecar
	// on exit. Byte count is O(1) (fstat); line count reads only the delta bytes
	// since the last sample (~1MB/60s typical), <10ms/tick.
	activityFile := utils.ShellQuote(path.Join(t.TaskDir, "activity.tsv"))
	logFileQuoted := utils.ShellQuote(t.LogPath)
	fmt.Fprintf(&s, "ACTIVITY_FILE=%s\n", activityFile)
	fmt.Fprintf(&s, "LOG_FILE=%s\n", logFileQuoted)
	s.WriteString("_RUNQ_PREV_BYTES=0\n")
	s.WriteString("_RUNQ_CUM_LINES=0\n")
	s.WriteString("(while sleep 60; do\n")
	s.WriteString(`  _CUR=$(stat -c%s "$LOG_FILE" 2>/dev/null || echo 0)` + "\n")
	s.WriteString("  _D=$((_CUR - _RUNQ_PREV_BYTES))\n")
	s.WriteString(`  [ "$_D" -gt 0 ] && _RUNQ_CUM_LINES=$((_RUNQ_CUM_LINES + \` + "\n")
	s.WriteString(`    $(dd if="$LOG_FILE" bs=65536 skip=$_RUNQ_PREV_BYTES count=$_D \` + "\n")
	s.WriteString(`      iflag=skip_bytes,count_bytes 2>/dev/null | tr -cd '\n' | wc -c)))` + "\n")
	s.WriteString(`  printf '%s\t%s\t%s\n' "$(date +%s)" "$_CUR" "$_RUNQ_CUM_LINES" >> "$ACTIVITY_FILE"` + "\n")
	s.WriteString("  _RUNQ_PREV_BYTES=$_CUR\n")
	s.WriteString("done) &\n")
	s.WriteString("_RUNQ_ACTIVITY_PID=$!\n")
	// Tasks run from the project's working_dir (daemon/HPC parity) —
	// schedulers start jobs in $HOME, where relative script paths break.
	wd := utils.ShellQuote(t.WorkingDir)
	fmt.Fprintf(&s, "if cd %s; then\n", wd)
	s.WriteString(t.Command + "\n")
	s.WriteString("code=$?\n")
	fmt.Fprintf(&s, "else\n  echo \"runq: cannot cd into working_dir %s\"\n  code=1\nfi\n", wd)
	s.WriteString("kill $_RUNQ_ACTIVITY_PID 2>/dev/null\n")
	s.WriteString(`if [ "$code" -eq 0 ]; then _st=success; else _st=failed; fi` + "\n")
	s.WriteString(`_runq_status "$(printf '{"status":"%s","exit_code":%s,"finished_at":%s}' "$_st" "$code" "$(date +%s)")"` + "\n")
	// Metrics pyramid: built HERE, on the compute node, where metrics.jsonl
	// lives — the login node never carries this. Before the done marker so
	// a marker's presence implies the index (when configured) had its
	// chance; `|| true` because an index is an accelerator and must never
	// pollute the task's terminal status. Skipped in the TERM trap: the
	// kill grace period is for the status write.
	s.WriteString(`if [ -n "$RUNQ_BIN" ]; then "$RUNQ_BIN" metrics-index build --task-dir "$RUNQ_TASK_DIR" >/dev/null 2>&1 || true; fi` + "\n")
	s.WriteString("_runq_done\n")
	s.WriteString("exit $code\n")
	return s.String()
}

// planToTaskRow maps a PlannedTask to a store.TaskRow for the HPC store. Status
// starts "pending"; refresh advances it. EnvJSON holds the user/project env
// (not RUNQ_*, which live in run.sh) to mirror the daemon's persisted shape.
func planToTaskRow(t submitplan.PlannedTask, plan submitplan.Plan, now time.Time, extID, target, generation string) store.TaskRow {
	paramsJSON, _ := json.Marshal(t.Params)
	envJSON, _ := json.Marshal(t.Env)
	return store.TaskRow{
		ID: t.TaskID, JobID: plan.JobID, ProjectName: plan.Project,
		Command: t.Command, ParamsJSON: string(paramsJSON), GPUsNeeded: t.GPUsNeeded,
		Status: "pending", MaxRetry: t.MaxRetry, LogPath: t.LogPath,
		WorkingDir: t.WorkingDir, EnvJSON: string(envJSON),
		Resumable: t.Resumable, ExtraArgs: t.ExtraArgs,
		UID: t.UID, Timeout: t.Timeout, EnqueuedAt: now,
		TaskDir: t.TaskDir, ExternalID: extID, Target: target,
		// RQ-75: stamp lane-generation ownership at creation — the row is
		// born owned by the generation that will submit it.
		TargetGeneration: generation,
	}
}
