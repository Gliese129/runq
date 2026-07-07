package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/preflight"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/runqenv"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/submitplan"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gliese129/runq/internal/workspace"
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

// submitEnvPrefix turns the project's environment into `export K='v'; `
// statements prefixed onto the submit command, so $TSUBAME_GROUP-style
// references in submit_template resolve from project config — not from
// whatever the login shell happens to export.
//
// NOT the `K='v' cmd $K` form: POSIX expands $K on the same command line
// BEFORE the assignment takes effect, so the reference would see the old
// (usually empty) value. `export K='v'; cmd $K` runs the assignment as its
// own command first; the whole string goes through `sh -c`, so nothing
// leaks into the calling shell.
func submitEnvPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(utils.ShellQuote(env[k]))
		b.WriteString("; ")
	}
	return b.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
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
// plan, validate, one-shot setup, job row, and — per task — workspace files,
// run.sh, submit.cmd, and the pending task row. The returned rows are ready
// to be queued; the actual submission happens per task in Launcher.Launch
// (scheduler lane) or inline in Submit (legacy lane).
func (b *Backend) Prepare(ctx context.Context, jobCfg job.JobConfig, proj *project.Config, opts SubmitOpts) (jobID string, rows []store.TaskRow, err error) {
	// Satisfy the jobs.project_name foreign key: ensure the project exists in
	// the HPC store. Add-if-missing covers both "resolved from registry" and
	// "loaded from a file" callers.
	reg := project.NewRegistry(b.Store.DB())
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
	// Workspace root: a configured target workspace WINS — it is a path on
	// the TARGET's filesystem, composed as POSIX and materialized via b.FS.
	// ResolveRoot is the on-target/legacy fallback only; it resolves against
	// the CLIENT machine (and even mkdirs locally), which is wrong for any
	// remote target.
	var wsRoot string
	if b.Cfg.Workspace != "" {
		wsRoot = b.Cfg.Workspace
	} else {
		var rerr error
		wsRoot, rerr = config.ResolveRoot(b.StorageCfg, proj.WorkingDir, proj.ProjectName)
		if rerr != nil {
			return "", nil, fmt.Errorf("resolve workspace root: %w", rerr)
		}
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

	disableLocal := b.Cfg.PreflightLocal != nil && !*b.Cfg.PreflightLocal
	plan, err := submitplan.Build(ctx, planCfg, proj, submitplan.Deps{
		JobID: jobID,
		IDGen: utils.GenerateTaskID,
		Paths: submitplan.Paths{
			WorkspaceRoot: jobRoot,
			LogRoot:       jobRoot,
		},
		SkipPreflight:         opts.SkipPreflight,
		PreflightDisableLocal: disableLocal,
		// Checks run against the TARGET: paths/script via its FS, probes on
		// its login node. The scope label is the honest caveat that the
		// login-node env may still differ from compute nodes.
		PreflightScope:  fmt.Sprintf("on %s login node", b.Cfg.Name),
		PreflightFS:     b.FS,
		SchedulerParams: config.HPCTemplateParamRefs(b.Cfg.SubmitTemplate),
	})
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
	if err := submitplan.RunSetup(ctx, proj, jobCfg); err != nil {
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
	if err := b.Store.InsertJob(ctx, &jobRow); err != nil {
		return "", nil, fmt.Errorf("persist job: %w", err)
	}

	// Ledger hygiene: if per-task preparation aborts midway, rows already
	// inserted would otherwise rot as pending forever — they never reach the
	// queue. The user sees the submit error; the ledger must agree with it.
	defer func() {
		if err == nil {
			return
		}
		for _, r := range rows {
			_ = b.Store.UpdateTaskStatus(ctx, r.ID, "failed",
				map[string]any{"finished_at": nowUnix(), "status_source": SourceSubmit})
		}
		_ = b.refreshJobStatus(ctx, jobID)
	}()

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
		fullCmd := submitEnvPrefix(proj.Environment) + cmd

		// Persist the rendered command next to run.sh. The scheduler-driven
		// launch path (remote.Launcher) replays this file verbatim, so retries
		// re-run exactly what was validated here. 0o600: may embed env values.
		if err := b.FS.WriteFile(path.Join(t.TaskDir, submitCmdFileName), []byte(fullCmd), 0o600); err != nil {
			return plan.JobID, rows, fmt.Errorf("write submit.cmd for %s: %w", t.TaskID, err)
		}

		// Durable ledger BEFORE the external submit: insert the task (pending, no
		// external_id yet). A sbatch that succeeds but whose id we then fail to
		// parse/persist must NOT become an invisible orphan — the row already
		// exists and status/kill/collect can see it.
		row := planToTaskRow(t, plan, now, "", b.Cfg.Name)
		if err := b.Store.InsertTask(ctx, &row); err != nil {
			return plan.JobID, rows, fmt.Errorf("persist task %s: %w", t.TaskID, err)
		}
		rows = append(rows, row)
	}

	return plan.JobID, rows, nil
}

// Submit is the LEGACY inline path: Prepare followed by synchronous per-task
// submission. The scheduler lane (SSHBackend queue + remote.Launcher) replaces
// it in the daemon; this remains for tests and non-daemon callers and will be
// removed in Step 3 of the scheduler unification.
func (b *Backend) Submit(ctx context.Context, jobCfg job.JobConfig, proj *project.Config, opts SubmitOpts) (jobID string, submitted int, err error) {
	jobID, rows, err := b.Prepare(ctx, jobCfg, proj, opts)
	if err != nil {
		return jobID, 0, err
	}
	for _, row := range rows {
		cmdBytes, rerr := b.FS.ReadFile(path.Join(row.TaskDir, submitCmdFileName))
		if rerr != nil {
			return jobID, submitted, fmt.Errorf("read submit.cmd for %s: %w", row.ID, rerr)
		}
		fullCmd := string(cmdBytes)

		out, xerr := b.shellRun(ctx, fullCmd)
		if xerr != nil {
			// sbatch itself errored → no cluster job exists → truly terminal.
			opLog("SUBMIT FAIL task=%s job=%s\ncmd: %s\nerr: %v\noutput: %s", row.ID, jobID, fullCmd, xerr, out)
			_ = b.Store.UpdateTaskStatus(ctx, row.ID, "failed",
				map[string]any{"finished_at": nowUnix(), "status_source": SourceSubmit})
			return jobID, submitted, fmt.Errorf("submit %s failed: %w\noutput:\n%s", row.ID, xerr, out)
		}

		extID, perr := utils.ExtractSubmitID(out, b.Cfg.SubmitIDRegex)
		if perr != nil {
			opLog("SUBMIT NOID task=%s job=%s\ncmd: %s\noutput: %s", row.ID, jobID, fullCmd, out)
			// Submit SUCCEEDED but the id is unparseable → a cluster job may be
			// running untracked. LEAVE the task pending (already inserted): it is
			// not a failure, and pending is non-terminal so reconcile won't stamp a
			// bogus finished_at. If the job actually runs it self-reports via
			// status.json and Reconcile heals pending→running/success. We just
			// can't kill it (no external id). Surface the error so the user fixes
			// submit_id_regex.
			return jobID, submitted, fmt.Errorf(
				"submitted %s but could not parse its job id — check submit_id_regex (the cluster job may be running untracked and is not killable without its id): %w\noutput:\n%s",
				row.ID, perr, out)
		}

		if uerr := b.Store.UpdateTaskStatus(ctx, row.ID, "pending", map[string]any{"external_id": extID}); uerr != nil {
			return jobID, submitted, fmt.Errorf("record external id for %s: %w", row.ID, uerr)
		}
		opLog("SUBMIT OK task=%s job=%s ext=%s\ncmd: %s", row.ID, jobID, extID, fullCmd)
		submitted++
	}
	return jobID, submitted, nil
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
	for k, v := range runqenv.Base(runqenv.Identity{
		TaskID: t.TaskID, JobID: plan.JobID, Project: plan.Project, TaskDir: t.TaskDir,
		SweepKeys: plan.SweepKeys, JobNote: plan.Note,
	}, runqenv.Safety{FactorPercent: 110}) {
		env[k] = v
	}
	env["RUNQ_NO_DAEMON"] = "1"
	// Done-marker dir: enables O(1) completion detection (one readdir instead
	// of N status.json reads). Only baked in when the target has a workspace
	// root; without it the daemon falls back to probe-only reconcile.
	if dir := b.doneDir(); dir != "" {
		env["RUNQ_DONE_DIR"] = dir
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
	if envFile := env["RUNQ_ENV_FILE"]; envFile != "" {
		q := utils.ShellQuote(envFile)
		fmt.Fprintf(&s, "if [ -f %s ]; then set -a; . %s; set +a; fi\n", q, q)
	}
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&s, "export %s=%s\n", k, utils.ShellQuote(env[k]))
	}
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
	s.WriteString("_runq_done\n")
	s.WriteString("exit $code\n")
	return s.String()
}

// planToTaskRow maps a PlannedTask to a store.TaskRow for the HPC store. Status
// starts "pending"; refresh advances it. EnvJSON holds the user/project env
// (not RUNQ_*, which live in run.sh) to mirror the daemon's persisted shape.
func planToTaskRow(t submitplan.PlannedTask, plan submitplan.Plan, now time.Time, extID, target string) store.TaskRow {
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
	}
}
