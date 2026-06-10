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

	"github.com/gliese129/runq/internal/hpccore"
	"github.com/gliese129/runq/internal/ingest"
	"github.com/gliese129/runq/internal/store"
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

// Refresh is the ONLY engine that advances task state — there is no resident
// process on HPC, so state moves forward only when a command calls this. It is a
// best-effort projection (see the package doc). For each task it ingests
// metrics.jsonl (always, idempotent — catches late/appended files), then, unless
// the task is already a HARD terminal, reads status.json + probes the scheduler
// and recomputes the canonical status+source via hpccore.Reconcile.
//
// Terminal handling keys off status_source:
//   - wrapper / runq terminals are FINAL — skipped (no re-probe).
//   - scheduler / submit / inferred terminals stay correctable: Refresh
//     re-probes every cycle so a late wrapper signal can overturn them
//     ("wrapper terminal wins").
//
// The DB is the (lazily-updated) source of truth and this is its only writer;
// there is no user-visible sync — status/best/collect call Refresh internally.
func (b *Backend) Refresh(ctx context.Context, jobID string) error {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
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
		if isTerminal(tk.Status) && (tk.StatusSource == hpccore.SourceWrapper || tk.StatusSource == hpccore.SourceRunq) {
			continue
		}

		sf := readStatus(tk.TaskDir)
		d := hpccore.Reconcile(tk.Status, tk.StatusSource, hpccore.Observed{
			WrapperStatus: sf.Status,
			ExitCode:      sf.ExitCode,
			Scheduler:     b.probeScheduler(ctx, tk.ExternalID),
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
	if len(ingestErrs) > 0 {
		return fmt.Errorf("status refreshed but ingest had errors: %w", errors.Join(ingestErrs...))
	}
	return nil
}

// probeScheduler runs the configured status_template (and optional status_parser
// hook) and maps the result to a semantic SchedulerSignal. It is deliberately
// dialect-agnostic:
//   - no status_template / no ext id / any command error → SchedUnknown (no new fact)
//   - with a status_parser hook → its normalized token via hpccore.ParseSignal
//   - without a hook → empty output = SchedGone, recognized token = that signal,
//     present-but-unrecognized = SchedActive (alive)
func (b *Backend) probeScheduler(ctx context.Context, extID string) hpccore.SchedulerSignal {
	if b.Cfg.StatusTemplate == "" || extID == "" {
		return hpccore.SchedUnknown
	}
	cmd, err := hpccore.Render(b.Cfg.StatusTemplate, map[string]string{"ext_id": extID})
	if err != nil {
		return hpccore.SchedUnknown
	}
	// Note: status command exit code handling differs by mode (below). Capture
	// the output regardless of error so a parser can still interpret it.
	out, runErr := b.Run(ctx, cmd)

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
			rs, perr := hpccore.Render(s, map[string]string{"ext_id": extID})
			if perr != nil {
				return hpccore.SchedUnknown
			}
			stages = append(stages, rs)
		}
		pipeline := "printf '%s\\n' " + hpccore.ShellQuote(out) + " | " + strings.Join(stages, " | ")
		pout, perr := b.Run(ctx, pipeline)
		if perr != nil {
			return hpccore.SchedUnknown
		}
		// Empty parser output = job absent from the active query = gone.
		if strings.TrimSpace(pout) == "" {
			return hpccore.SchedGone
		}
		return hpccore.ParseSignal(pout)
	}

	// No parser: a status command error is "no info" (don't guess).
	if runErr != nil {
		return hpccore.SchedUnknown
	}
	if strings.TrimSpace(out) == "" {
		return hpccore.SchedGone
	}
	if sig := hpccore.ParseSignal(out); sig != hpccore.SchedUnknown {
		return sig
	}
	return hpccore.SchedActive
}

// JobView is the read model returned to the CLI.
type JobView struct {
	Job   *store.JobRow
	Tasks []store.TaskRow
}

// Status refreshes the job, then returns its current DB state.
func (b *Backend) Status(ctx context.Context, jobID string) (*JobView, error) {
	if err := b.Refresh(ctx, jobID); err != nil {
		return nil, err
	}
	job, err := b.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return &JobView{Job: job, Tasks: tasks}, nil
}
