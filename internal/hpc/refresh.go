package hpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// statusFile is the shape run.sh writes to <task_dir>/status.json.
type statusFile struct {
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

// readStatus reads and parses status.json. A missing or malformed file yields a
// zero statusFile (WrapperStatus == ""), which Reconcile treats as "no terminal
// signal yet".
func readStatus(taskDir string) statusFile {
	var sf statusFile
	buf, err := os.ReadFile(filepath.Join(taskDir, statusFileName))
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
	probe := ttl == 0 || !b.probeIsFresh(jobID, ttl)
	return b.reconcile(ctx, jobID, probe)
}

// EnsureAllFresh reconciles all active (non-done) jobs within the TTL window.
func (b *Backend) EnsureAllFresh(ctx context.Context, ttl time.Duration) error {
	jobs, err := b.Store.ListJobs(ctx, "")
	if err != nil {
		return err
	}
	var firstErr error
	for _, j := range jobs {
		if j.Status == "done" {
			continue // all tasks terminal, nothing to reconcile
		}
		if err := b.EnsureFresh(ctx, j.ID, ttl); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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

// reconcile is the core engine that advances task state. For each task it
// ingests metrics.jsonl (always, idempotent), then reads status.json and
// optionally probes the scheduler, recomputing the canonical status+source
// via Reconcile.
//
// When probe=true the scheduler is queried (status_template); when false,
// only local file reads run (status.json + metrics.jsonl). The TTL cache
// in EnsureFresh decides which mode to use — callers of reconcile should
// not need to worry about this.
//
// Terminal handling keys off status_source:
//   - wrapper / runq terminals are FINAL — skipped (no re-probe).
//   - scheduler / submit / inferred terminals stay correctable: reconcile
//     re-probes every cycle so a late wrapper signal can overturn them
//     ("wrapper terminal wins").
//
// The DB is the (lazily-updated) source of truth and this is its only writer.
func (b *Backend) reconcile(ctx context.Context, jobID string, probe bool) error {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	// Batch effect for listing-style status templates: presets like SGE/UGE
	// run a FULL `qstat` and select the task's row in the parser (awk). The
	// rendered status command is then identical for every task — memoize it
	// for this pass, so N tasks cost ONE scheduler query. Templates that
	// embed {{ext_id}} render uniquely per task and behave as before.
	probeRun := memoRunner(b.Run)
	var ingestErrs []error
	for _, tk := range tasks {
		// Metrics/checkpoints: idempotent (INSERT OR IGNORE), run for every task
		// so files that appear/grow after a status change still land.
		if _, ierr := ingest.ReapOutputs(ctx, b.Store, ingest.Target{
			TaskID: tk.ID, JobID: tk.JobID, Dir: tk.TaskDir,
		}); ierr != nil {
			ingestErrs = append(ingestErrs, fmt.Errorf("ingest task %s: %w", tk.ID, ierr))
		}

		// Only wrapper and runq terminals are hard-final (skip re-probe).
		// Scheduler / submit / inferred terminals stay correctable so a late
		// wrapper signal can overturn them.
		if isTerminal(tk.Status) && (tk.StatusSource == SourceWrapper || tk.StatusSource == SourceRunq) {
			continue
		}

		sf := readStatus(tk.TaskDir)
		schedSignal := SchedUnknown
		if probe {
			schedSignal = b.probeSchedulerWith(ctx, probeRun, tk.ExternalID)
		}
		d := Reconcile(tk.Status, tk.StatusSource, Observed{
			WrapperStatus: sf.Status,
			ExitCode:      sf.ExitCode,
			Scheduler:     schedSignal,
			KillRequested: false, // hpc kill writes killed/runq directly; never infer intent from status
		})

		if d.Status == tk.Status && d.Source == tk.StatusSource {
			continue // nothing changed
		}

		fields := map[string]any{"status_source": d.Source}
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

		if err := b.Store.UpdateTaskStatus(ctx, tk.ID, d.Status, fields); err != nil {
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

// ── Scheduler probing ──

// probeScheduler runs the configured status_template (and optional status_parser
// hook) and maps the result to a semantic SchedulerSignal. It is deliberately
// dialect-agnostic:
//   - no status_template / no ext id / any command error → SchedUnknown (no new fact)
//   - with a status_parser hook → its normalized token via ParseSignal
//   - without a hook → empty output = SchedGone, recognized token = that signal,
//     present-but-unrecognized = SchedActive (alive)
func (b *Backend) probeScheduler(ctx context.Context, extID string) SchedulerSignal {
	return b.probeSchedulerWith(ctx, b.Run, extID)
}

func (b *Backend) probeSchedulerWith(ctx context.Context, run Runner, extID string) SchedulerSignal {
	if b.Cfg.StatusTemplate == "" || extID == "" {
		return SchedUnknown
	}
	cmd, err := utils.Render(b.Cfg.StatusTemplate, map[string]string{"ext_id": extID})
	if err != nil {
		return SchedUnknown
	}
	// Note: status command exit code handling differs by mode (below). Capture
	// the output regardless of error so a parser can still interpret it.
	out, runErr := run(ctx, cmd)

	// Optional parser pipeline: feed the status output to stage 1 on stdin and
	// pipe each stage into the next. runq assembles the pipe so each stage stays
	// a short filter; {{ext_id}} is available in any stage.
	//
	// When a parser is configured we do NOT bail on a non-zero status command:
	// many active queries (e.g. `qstat -f <finished_id>`) ERROR once the job
	// leaves the queue, and that error IS the "gone" signal. We hand the (often
	// empty) output to the parser and let it decide — the parser is contracted to
	// emit `gone` for absence. (Trade-off: a genuinely broken status binary that
	// returns empty will also look "gone"; for reliable terminal states use an
	// accounting query like the slurm preset's sacct.)
	if len(b.Cfg.StatusParser) > 0 {
		stages := make([]string, 0, len(b.Cfg.StatusParser))
		for _, s := range b.Cfg.StatusParser {
			rs, perr := utils.Render(s, map[string]string{"ext_id": extID})
			if perr != nil {
				return SchedUnknown
			}
			stages = append(stages, rs)
		}
		pipeline := "printf '%s\\n' " + utils.ShellQuote(out) + " | " + strings.Join(stages, " | ")
		pout, perr := run(ctx, pipeline)
		if perr != nil {
			return SchedUnknown
		}
		// Empty parser output = job absent from the active query = gone.
		if strings.TrimSpace(pout) == "" {
			return SchedGone
		}
		return ParseSignal(pout)
	}

	// No parser: a status command error is "no info" (don't guess).
	if runErr != nil {
		return SchedUnknown
	}
	if strings.TrimSpace(out) == "" {
		return SchedGone
	}
	if sig := ParseSignal(out); sig != SchedUnknown {
		return sig
	}
	return SchedActive
}

// memoRunner caches command → result for the lifetime of one reconcile pass.
// Status queries are idempotent reads, so within a pass the same rendered
// command need not hit the scheduler twice. Sequential use only (no lock).
func memoRunner(run Runner) Runner {
	type result struct {
		out string
		err error
	}
	cache := map[string]result{}
	return func(ctx context.Context, command string) (string, error) {
		if r, ok := cache[command]; ok {
			return r.out, r.err
		}
		out, err := run(ctx, command)
		cache[command] = result{out, err}
		return out, err
	}
}
