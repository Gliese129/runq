package hpc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// Refresh projects on-disk task state into the DB. For each task it:
//   - ingests metrics.jsonl (idempotent — safe to re-run every refresh)
//   - reads status.json and optionally probes the scheduler
//   - asks hpccore.Reconcile for the canonical status and writes it
//
// The DB is the source of truth; this is the only writer. There is no
// user-visible sync — status/best/collect call Refresh internally.
func (b *Backend) Refresh(ctx context.Context, jobID string) error {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	for _, tk := range tasks {
		// Metrics/checkpoints: idempotent batch insert (INSERT OR IGNORE).
		_, _ = ingest.ReapOutputs(ctx, b.Store, ingest.Target{
			TaskID: tk.ID, JobID: tk.JobID, Dir: tk.TaskDir,
		})

		sf := readStatus(tk.TaskDir)

		canonical := hpccore.Reconcile(tk.Status, hpccore.Observed{
			WrapperStatus: sf.Status,
			ExitCode:      sf.ExitCode,
			Scheduler:     b.probeScheduler(ctx, tk.ExternalID),
			KillRequested: tk.Status == "killed", // DB stays authoritative
		})

		fields := map[string]any{}
		// Stamp finished_at once, on the first terminal transition, preferring
		// the wrapper's timestamp; never re-stamp (avoids drift across refreshes).
		if isTerminal(canonical) && tk.FinishedAt == nil {
			ft := sf.FinishedAt
			if ft == 0 {
				ft = nowUnix()
			}
			fields["finished_at"] = ft
		}

		if canonical == tk.Status && len(fields) == 0 {
			continue // nothing changed
		}
		if err := b.Store.UpdateTaskStatus(ctx, tk.ID, canonical, fields); err != nil {
			return fmt.Errorf("update task %s: %w", tk.ID, err)
		}
	}
	return b.refreshJobStatus(ctx, jobID)
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
	out, err := b.Run(ctx, cmd)
	if err != nil {
		return hpccore.SchedUnknown
	}

	// Optional parser pipeline: feed the raw output to stage 1 on stdin and pipe
	// each stage into the next. runq assembles the pipe so each stage stays a
	// short filter; {{ext_id}} is available in any stage.
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
		return hpccore.ParseSignal(pout)
	}

	// No hook: interpret the raw output directly.
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
