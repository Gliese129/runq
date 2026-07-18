package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gliese129/runq/internal/workspace"
)

// statusFile is the shape run.sh writes to <task_dir>/status.json.
type statusFile struct {
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

// readStatus reads and parses status.json via the given FS. A missing or
// malformed file yields a zero statusFile (WrapperStatus == ""), which
// Reconcile treats as "no terminal signal yet".
func readStatus(fsys rfs.FS, taskDir string) statusFile {
	var sf statusFile
	buf, err := fsys.ReadFile(path.Join(taskDir, statusFileName))
	if err != nil {
		return sf
	}
	_ = json.Unmarshal(buf, &sf)
	return sf
}

// ── Reconciler (satisfies backend.Reconciler via duck typing) ──

// EnsureFresh ensures jobID's data is current. Local reconcile (status.json,
// metrics.jsonl ingest) ALWAYS runs. The scheduler probe runs only when ttl=0
// (force) or when the probe cache has expired. This is the ONLY entry point
// for advancing HPC task state — there is no resident process, so state moves
// forward only when a command calls this.
//
// Two-tier design:
//   - Local file reads (cheap): always run so wrapper-written status/metrics
//     surface immediately.
//   - Scheduler probe (expensive, may ssh/qstat): TTL-gated.
//
// The method satisfies backend.Reconciler implicitly (Go structural typing).
func (b *Backend) EnsureFresh(ctx context.Context, jobID string, ttl time.Duration) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	probe := ttl == 0 || !b.probeIsFresh(jobID, ttl)
	return b.reconcile(ctx, jobID, probe)
}

// SchedulerProbe is THE expensive verb of this backend — the one that
// talks to qstat/squeue. It reconciles all active (non-done) jobs: local
// wrapper reads always run; the scheduler query itself is floor-gated
// (floor=0 forces). Name says the cost: if you don't need the scheduler's
// opinion, you want HeartbeatProbe or ScanDoneMarkers instead.
//
// When status_list_template is configured, a single batch probe replaces all
// per-job scheduler queries — one `qstat -u $USER` (or equivalent) call serves
// every active job. Without it, a shared memoRunner still deduplicates
// listing-style per-job commands (e.g. SGE/UGE bare `qstat`) across jobs.
func (b *Backend) SchedulerProbe(ctx context.Context, floor time.Duration) error {
	ttl := floor
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	jobs, err := b.Store.ListJobs(ctx, "", b.Cfg.Name)
	if err != nil {
		return err
	}

	// Pre-scan: which jobs need a scheduler probe this pass?
	type entry struct {
		id    string
		probe bool
	}
	var active []entry
	anyNeedsProbe := false
	for _, j := range jobs {
		if store.IsTerminalJobStatus(j.Status) {
			continue
		}
		probe := ttl == 0 || !b.probeIsFresh(j.ID, ttl)
		active = append(active, entry{j.ID, probe})
		if probe {
			anyNeedsProbe = true
		}
	}

	// Batch probe: only run when (a) configured, (b) at least one job needs
	// a probe, AND (c) the batch TTL itself has expired. This prevents
	// hitting qstat/squeue on every poll when everything is still fresh.
	var batchSignals map[string]ProbeResult
	if anyNeedsProbe && b.Cfg.StatusListTemplate != "" {
		if !b.batchProbeIsFresh(ttl) {
			signals, berr := b.probeBatch(ctx)
			if signals != nil && berr == nil {
				batchSignals = signals
				b.markBatchProbed()
			}
			// On batch failure: batchSignals stays nil → fall back to
			// per-job probing below. No error propagation — the per-job
			// path is the safe fallback.
		}
	}

	// Fallback: shared memoRunner deduplicates identical commands across jobs.
	sharedRun := memoRunner(b.shellRunClassified)

	var firstErr error
	for _, e := range active {
		var rerr error
		if batchSignals != nil && e.probe {
			rerr = b.reconcileWithBatch(ctx, e.id, batchSignals)
		} else {
			rerr = b.reconcileWith(ctx, e.id, e.probe, sharedRun)
		}
		if rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}

// HeartbeatProbe is the FILE-STREAM liveness pass — zero scheduler
// commands, stat only. A running task proves it is alive by writing:
// stdout log, activity.tsv, metrics.jsonl. The freshest mtime across
// those streams is the heartbeat; a task silent longer than silenceAfter
// (baseline: its started_at) lands in the returned list, and the CALLER
// decides whether that suspicion is worth a SchedulerProbe. Cost model:
// N running tasks = ≤3N stats over the existing connection.
func (b *Backend) HeartbeatProbe(ctx context.Context, silenceAfter time.Duration) (silent []string, err error) {
	rows, err := b.Store.ListTasks(ctx, store.TaskFilter{Status: "running", Target: b.Cfg.Name})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, tk := range rows {
		if tk.TaskDir == "" {
			continue
		}
		var last time.Time
		if tk.StartedAt != nil {
			last = *tk.StartedAt
		}
		streams := []string{tk.LogPath, workspace.ActivityPath(tk.TaskDir), workspace.MetricsPath(tk.TaskDir)}
		for _, p := range streams {
			if p == "" {
				continue
			}
			if fi, serr := b.FS.Stat(p); serr == nil && fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
		if last.IsZero() {
			continue // no baseline at all — cannot judge silence
		}
		if now.Sub(last) > silenceAfter {
			silent = append(silent, tk.ID)
		}
	}
	return silent, nil
}

// awaitingRelaunch reports whether the task was requeued by the scheduler
// (retry) and has not been relaunched yet: no external id, at least one prior
// attempt. Wrapper files on disk belong to the PREVIOUS attempt and must not
// be reconciled against — remote.Launcher resets them at the next launch.
func awaitingRelaunch(tk store.TaskRow) bool {
	// Only PENDING rows can be "requeued but not relaunched". An `unknown`
	// task (RQ-74) also has no external id and may have retry_count > 0, but
	// it is exactly the task reconcile must look at — its on-disk wrapper
	// state is the evidence that settles it.
	return tk.Status == "pending" && tk.ExternalID == "" && tk.RetryCount > 0
}

// persistDecision writes a reconcile decision. A terminal transition of a
// not-yet-terminal task goes through the scheduler's FinishTask funnel when a
// Finisher is wired (retry policy, slot release, queue bookkeeping); every
// other write — non-terminal moves, display-field refreshes, terminal-to-
// terminal corrections (inferred→wrapper) — stays a direct store update,
// because the scheduler only manages in-flight tasks.
func (b *Backend) persistDecision(ctx context.Context, tk store.TaskRow, d Decision, fields map[string]any) error {
	// INVARIANT (RQ-65 #5), enforced at THE funnel — every hard-final
	// write from every entry point (reconcile, batch, marker scan, kill)
	// passes through here, and a hard-final is the LAST write a task ever
	// sees: whatever it leaves behind is frozen forever. Frozen fields
	// must hold TIMELESS values, not snapshots — even a freshly probed
	// "running"/"COMPLETING" is a truth with seconds to live. So the
	// override is unconditional: "gone" (the scheduler no longer knows
	// this job) is the only native_state that stays true.
	if isTerminal(d.Status) && (d.Source == SourceWrapper || d.Source == SourceRunq) {
		fields["native_state"] = "gone"
		fields["queue"] = ""
	}
	if b.Finisher != nil && isTerminal(d.Status) && !isTerminal(tk.Status) {
		b.Finisher.FinishTask(rowToTask(tk), scheduler.TaskStatus(d.Status), fields)
		return nil
	}
	return b.Store.UpdateTaskStatus(ctx, tk.ID, d.Status, fields)
}

// probeIsFresh returns true if jobID's scheduler was probed within the TTL.
func (b *Backend) probeIsFresh(jobID string, ttl time.Duration) bool {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if b.lastProbe == nil {
		return false
	}
	return time.Since(b.lastProbe[jobID]) < ttl
}

// markProbed records that jobID's scheduler was just probed.
func (b *Backend) markProbed(jobID string) {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if b.lastProbe == nil {
		b.lastProbe = make(map[string]time.Time)
	}
	b.lastProbe[jobID] = time.Now()
}

// batchProbeIsFresh returns true if the global batch probe ran within the TTL.
func (b *Backend) batchProbeIsFresh(ttl time.Duration) bool {
	if ttl == 0 {
		return false // force refresh
	}
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	return !b.lastBatchProbe.IsZero() && time.Since(b.lastBatchProbe) < ttl
}

// markBatchProbed records that a global batch probe just completed.
func (b *Backend) markBatchProbed() {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	b.lastBatchProbe = time.Now()
}

// reconcile is the core engine that advances task state. Creates a fresh
// memoRunner for the single-job case; SchedulerProbe calls reconcileWith
// directly with a shared runner.
func (b *Backend) reconcile(ctx context.Context, jobID string, probe bool) error {
	return b.reconcileWith(ctx, jobID, probe, memoRunner(b.shellRunClassified))
}

// reconcileWith is the core engine that advances task state. For each task it
// ingests metrics.jsonl (always, idempotent), then reads status.json and
// optionally probes the scheduler, recomputing the canonical status+source
// via Reconcile.
//
// When probe=true the scheduler is queried (status_template); when false,
// only local file reads run (status.json + metrics.jsonl). The TTL cache
// in EnsureFresh decides which mode to use — callers of reconcileWith should
// not need to worry about this.
//
// Terminal handling keys off status_source:
//   - wrapper / runq terminals are FINAL — skipped (no re-probe).
//   - scheduler / submit / inferred terminals stay correctable: reconcile
//     re-probes every cycle so a late wrapper signal can overturn them
//     ("wrapper terminal wins").
//
// The DB is the (lazily-updated) source of truth and this is its only writer.
//
// The probeRun argument is a (possibly memo-cached) runner: SchedulerProbe
// shares one across all jobs so listing-style commands (bare `qstat`) hit
// the scheduler once; single-job callers pass memoRunner(b.shellRunClassified).
func (b *Backend) reconcileWith(ctx context.Context, jobID string, probe bool, probeRun runner) error {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	var ingestErrs []error
	for _, tk := range tasks {
		// Metrics/checkpoints: incremental via the (size,offset) mark —
		// growth ships only the delta, no growth is one stat. final only
		// for hard-final terminals (wrapper/runq source): soft terminals
		// can still be overturned and rerun.
		hardFinal := isTerminal(tk.Status) && (tk.StatusSource == SourceWrapper || tk.StatusSource == SourceRunq)
		if _, ierr := ingest.ReapIncremental(ctx, b.Store, ingest.Target{
			TaskID: tk.ID, JobID: tk.JobID, Dir: tk.TaskDir, FS: b.FS,
		}, hardFinal); ierr != nil {
			ingestErrs = append(ingestErrs, fmt.Errorf("ingest task %s: %w", tk.ID, ierr))
		}

		// Only wrapper and runq terminals are hard-final (skip re-probe).
		// Scheduler / submit / inferred terminals stay correctable so a late
		// wrapper signal can overturn them.
		if isTerminal(tk.Status) && (tk.StatusSource == SourceWrapper || tk.StatusSource == SourceRunq) {
			continue
		}
		// Requeued-but-not-relaunched: on-disk wrapper state is the previous
		// attempt's — reconciling against it would re-fail the task in a loop.
		if awaitingRelaunch(tk) {
			continue
		}

		sf := readStatus(b.FS, tk.TaskDir)
		var pr ProbeResult
		if probe {
			pr = b.probeSchedulerWith(ctx, probeRun, tk.ExternalID)
		}
		d := Reconcile(tk.Status, tk.StatusSource, Observed{
			WrapperStatus: sf.Status,
			ExitCode:      sf.ExitCode,
			Scheduler:     pr.Signal,
			KillRequested: false, // hpc kill writes killed/runq directly; never infer intent from status
		})

		// Build extra fields. native_state and queue are only written when
		// a probe ran — local-only reconcile must not overwrite existing values.
		fields := map[string]any{"status_source": d.Source}
		if probe && pr.NativeState != "" {
			fields["native_state"] = pr.NativeState
		}
		if probe && pr.Queue != "" {
			fields["queue"] = pr.Queue
		}

		statusChanged := d.Status != tk.Status || d.Source != tk.StatusSource
		hpcFieldsChanged := (probe && pr.NativeState != "" && pr.NativeState != tk.NativeState) ||
			(probe && pr.Queue != "" && pr.Queue != tk.Queue)
		if !statusChanged && !hpcFieldsChanged {
			continue // nothing changed
		}

		switch {
		case isTerminal(d.Status) && d.Status != tk.Status:
			// Entering a terminal, or correcting one terminal to another
			// (inferred→wrapper): (re)stamp finished_at, preferring the wrapper's.
			if sf.FinishedAt > 0 {
				fields["finished_at"] = sf.FinishedAt
			} else {
				fields["finished_at"] = nowUnix()
			}
		case !isTerminal(d.Status) && isTerminal(tk.Status):
			// Leaving a terminal (an inferred failure the scheduler re-activated):
			// clear the stamp.
			fields["finished_at"] = nil
		}

		if err := b.persistDecision(ctx, tk, d, fields); err != nil {
			return fmt.Errorf("update task %s: %w", tk.ID, err)
		}
	}

	if err := b.refreshJobStatus(ctx, jobID); err != nil {
		return err
	}
	// Record that a reconcile pass completed. This is the honesty contract
	// of poll-based state: consumers (dashboard "data as of ...") can only
	// be truthful about staleness if the reconcile time is a recorded fact.
	if err := b.Store.TouchJobRefreshedAt(ctx, jobID, time.Now()); err != nil {
		return fmt.Errorf("touch refreshed_at for job %s: %w", jobID, err)
	}
	// Mark the in-memory TTL cache so subsequent calls within the window skip
	// the scheduler probe (local reconcile always runs regardless).
	if probe {
		b.markProbed(jobID)
	}
	if len(ingestErrs) > 0 {
		return fmt.Errorf("status refreshed but ingest had errors: %w", errors.Join(ingestErrs...))
	}
	return nil
}

// reconcileWithBatch is a variant of reconcileWith that uses pre-computed
// batch probe results instead of per-task scheduler queries. ext_ids absent
// from the batch map are treated as SchedGone (job left the scheduler queue).
func (b *Backend) reconcileWithBatch(ctx context.Context, jobID string, signals map[string]ProbeResult) error {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	var ingestErrs []error
	for _, tk := range tasks {
		hardFinal := isTerminal(tk.Status) && (tk.StatusSource == SourceWrapper || tk.StatusSource == SourceRunq)
		if _, ierr := ingest.ReapIncremental(ctx, b.Store, ingest.Target{
			TaskID: tk.ID, JobID: tk.JobID, Dir: tk.TaskDir, FS: b.FS,
		}, hardFinal); ierr != nil {
			ingestErrs = append(ingestErrs, fmt.Errorf("ingest task %s: %w", tk.ID, ierr))
		}

		if hardFinal {
			continue
		}
		// Requeued-but-not-relaunched: skip, same as reconcileWith.
		if awaitingRelaunch(tk) {
			continue
		}

		sf := readStatus(b.FS, tk.TaskDir)

		// Look up pre-computed probe result; absent ext_ids are gone.
		pr := ProbeResult{Signal: SchedUnknown}
		if tk.ExternalID != "" {
			if p, ok := signals[tk.ExternalID]; ok {
				pr = p
			} else {
				pr = ProbeResult{Signal: SchedGone}
			}
		}

		d := Reconcile(tk.Status, tk.StatusSource, Observed{
			WrapperStatus: sf.Status,
			ExitCode:      sf.ExitCode,
			Scheduler:     pr.Signal,
			KillRequested: false,
		})

		// Build extra fields. Batch probe always counts as probe=true.
		fields := map[string]any{"status_source": d.Source}
		if pr.NativeState != "" {
			fields["native_state"] = pr.NativeState
		}
		if pr.Queue != "" {
			fields["queue"] = pr.Queue
		}

		statusChanged := d.Status != tk.Status || d.Source != tk.StatusSource
		hpcFieldsChanged := (pr.NativeState != "" && pr.NativeState != tk.NativeState) ||
			(pr.Queue != "" && pr.Queue != tk.Queue)
		if !statusChanged && !hpcFieldsChanged {
			continue
		}

		switch {
		case isTerminal(d.Status) && d.Status != tk.Status:
			if sf.FinishedAt > 0 {
				fields["finished_at"] = sf.FinishedAt
			} else {
				fields["finished_at"] = nowUnix()
			}
		case !isTerminal(d.Status) && isTerminal(tk.Status):
			fields["finished_at"] = nil
		}

		if err := b.persistDecision(ctx, tk, d, fields); err != nil {
			return fmt.Errorf("update task %s: %w", tk.ID, err)
		}
	}

	if err := b.refreshJobStatus(ctx, jobID); err != nil {
		return err
	}
	if err := b.Store.TouchJobRefreshedAt(ctx, jobID, time.Now()); err != nil {
		return fmt.Errorf("touch refreshed_at for job %s: %w", jobID, err)
	}
	b.markProbed(jobID)
	if len(ingestErrs) > 0 {
		return fmt.Errorf("status refreshed but ingest had errors: %w", errors.Join(ingestErrs...))
	}
	return nil
}

// ── Scheduler probing ──

// probeBatch runs the status_list_template once and parses the output into a
// map of ext_id → SchedulerSignal. Returns (nil, nil) when no list template
// is configured — callers should fall back to per-job probing.
//
// Safety: a non-nil but EMPTY map is treated as an error (returned as nil),
// because "no jobs in output" most likely means the command failed silently
// or the parser dropped everything — not that every tracked job is gone.
// Callers seeing (nil, non-nil) should fall back to per-job probing.
func (b *Backend) probeBatch(ctx context.Context) (map[string]ProbeResult, error) {
	if b.Cfg.StatusListTemplate == "" {
		return nil, nil
	}
	out, runErr := b.shellRun(ctx, b.Cfg.StatusListTemplate)

	// Command failure is always an error — even when a parser is configured.
	// The per-job path (probeSchedulerWith) intentionally ignores command
	// errors when a parser exists (because e.g. `qstat -f <gone_id>` errors
	// as its "gone" signal). But the LIST command is never per-job — it
	// should always succeed when the scheduler is reachable.
	if runErr != nil {
		return nil, fmt.Errorf("status_list_template: %w", runErr)
	}

	if len(b.Cfg.StatusListParser) > 0 {
		// Feed raw output through the parser pipeline.
		pipeline := "printf '%s\\n' " + utils.ShellQuote(out) + " | " + strings.Join(b.Cfg.StatusListParser, " | ")
		pout, perr := b.shellRun(ctx, pipeline)
		if perr != nil {
			return nil, fmt.Errorf("status_list_parser: %w", perr)
		}
		out = pout
	}

	// Parse "ext_id signal [queue]" lines into a lookup map.
	result := make(map[string]ProbeResult)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		pr := ProbeResult{
			Signal:      MapSignal(b.Cfg, parts[1]),
			NativeState: parts[1],
		}
		if len(parts) >= 3 {
			pr.Queue = parts[2]
		}
		result[parts[0]] = pr
	}

	// Empty result: for dialect schedulers (qstat/squeue) this is suspicious
	// — the command "succeeded" but nothing parsed, so fall back to per-job
	// probing rather than declaring every job gone. Targets that TRUST their
	// list (runqd: squeue reads its own SQLite — empty means truly empty)
	// opt in via trust_empty_list and skip the wasteful fallback.
	if len(result) == 0 && !b.Cfg.TrustEmptyList {
		return nil, nil
	}
	return result, nil
}

// probeScheduler runs the configured status_template (and optional status_parser
// hook) and maps the result to a semantic SchedulerSignal. It is deliberately
// dialect-agnostic:
//   - no status_template / no ext id / any command error → SchedUnknown (no new fact)
//   - with a status_parser hook → its normalized token via ParseSignal
//   - without a hook → empty output = SchedGone, recognized token = that signal,
//     present-but-unrecognized = SchedActive (alive)
func (b *Backend) probeScheduler(ctx context.Context, extID string) ProbeResult {
	return b.probeSchedulerWith(ctx, b.shellRunClassified, extID)
}

func (b *Backend) probeSchedulerWith(ctx context.Context, run runner, extID string) ProbeResult {
	none := ProbeResult{Signal: SchedUnknown}
	if b.Cfg.StatusTemplate == "" || extID == "" {
		return none
	}
	cmd, err := utils.Render(b.Cfg.StatusTemplate, map[string]string{"ext_id": extID})
	if err != nil {
		return none
	}
	out, exitCode, runErr := run(ctx, cmd)
	// TRANSPORT failure: the command never ran — no fact learned, regardless
	// of parser config. Only an EXIT code may be read as a scheduler verdict
	// ("qstat errors once the job left the queue"); a connection blip parsed
	// as "gone" would get healthy tasks inferred as failed.
	if runErr != nil {
		return none
	}

	// Optional parser pipeline: feed the status output to stage 1 on stdin and
	// pipe each stage into the next. runq assembles the pipe so each stage stays
	// a short filter; {{ext_id}} is available in any stage.
	//
	// When a parser is configured we do NOT bail on a non-zero EXIT of the
	// status command: many active queries (e.g. `qstat -f <finished_id>`)
	// ERROR once the job leaves the queue, and that exit IS the "gone"
	// signal. We hand the (often empty) output to the parser and let it
	// decide — the parser is contracted to emit `gone` for absence.
	if len(b.Cfg.StatusParser) > 0 {
		stages := make([]string, 0, len(b.Cfg.StatusParser))
		for _, s := range b.Cfg.StatusParser {
			rs, perr := utils.Render(s, map[string]string{"ext_id": extID})
			if perr != nil {
				return none
			}
			stages = append(stages, rs)
		}
		pipeline := "printf '%s\\n' " + utils.ShellQuote(out) + " | " + strings.Join(stages, " | ")
		pout, pcode, perr := run(ctx, pipeline)
		if perr != nil || pcode != 0 {
			return none // the PIPELINE must succeed; its failure is never a verdict
		}
		token := strings.TrimSpace(pout)
		// Empty parser output = job absent from the active query = gone.
		if token == "" {
			return ProbeResult{Signal: SchedGone}
		}
		return ProbeResult{
			Signal:      MapSignal(b.Cfg, token),
			NativeState: token,
		}
	}

	// No parser: any non-zero exit is "no info" (don't guess).
	if exitCode != 0 {
		return none
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return ProbeResult{Signal: SchedGone}
	}
	sig := MapSignal(b.Cfg, token)
	if sig != SchedUnknown {
		return ProbeResult{Signal: sig, NativeState: token}
	}
	return ProbeResult{Signal: SchedActive, NativeState: token}
}

// memoRunner caches command → result for the lifetime of one reconcile pass.
// Status queries are idempotent reads, so within a pass the same rendered
// command need not hit the scheduler twice. Sequential use only (no lock).
func memoRunner(run runner) runner {
	type result struct {
		out  string
		code int
		err  error
	}
	cache := map[string]result{}
	return func(ctx context.Context, command string) (string, int, error) {
		if r, ok := cache[command]; ok {
			return r.out, r.code, r.err
		}
		out, code, err := run(ctx, command)
		cache[command] = result{out, code, err}
		return out, code, err
	}
}
