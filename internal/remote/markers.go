package remote

import (
	"context"
	"path"
	"strings"

	"github.com/gliese129/runq/internal/utils"
)

// ScanDoneMarkers is the cheap completion detector: ONE ReadDir of the
// target's done dir tells us which tasks finished since the last look,
// then only those tasks' status.json files are read (K reads for K new
// completions, not N reads for N in-flight tasks). This is the primary
// completion path; the scheduler probe (qstat) is the low-frequency
// alignment net for tasks whose wrapper never got to write a marker
// (node fail, hard kill).
//
// Ownership rule: markers whose task id is unknown to this store, or whose
// task belongs to another target, are LEFT IN PLACE — another runq client
// sharing this workspace may own them. We only ever delete what we settled.
func (b *Backend) ScanDoneMarkers(ctx context.Context) error {
	dir := b.doneDir()
	if dir == "" {
		return nil // no workspace root configured — marker detection disabled
	}
	entries, err := b.FS.ReadDir(dir)
	if err != nil {
		return nil // dir absent = nothing has finished yet; not an error
	}

	var cleanup []string
	jobs := make(map[string]struct{})
	var firstErr error

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id := e.Name()
		tk, gerr := b.Store.GetTask(ctx, id)
		if gerr != nil || tk == nil || tk.Target != b.Cfg.Name {
			continue // not ours — leave the marker alone
		}
		if isTerminal(tk.Status) {
			// Already settled (probe beat the marker, or a rescan): the
			// marker is stale bookkeeping, safe to clear.
			cleanup = append(cleanup, path.Join(dir, id))
			continue
		}
		if awaitingRelaunch(*tk) {
			continue // previous attempt's marker; Launch clears it
		}

		sf := readStatus(b.FS, tk.TaskDir)
		d := Reconcile(tk.Status, tk.StatusSource, Observed{
			WrapperStatus: sf.Status,
			ExitCode:      sf.ExitCode,
			Scheduler:     SchedUnknown, // marker path never probes
		})
		if !isTerminal(d.Status) {
			// Marker present but status.json unreadable/non-terminal — a
			// half-written state. Leave the marker; the probe-align pass
			// will settle it with scheduler facts.
			continue
		}

		fields := map[string]any{"status_source": d.Source}
		if sf.FinishedAt > 0 {
			fields["finished_at"] = sf.FinishedAt
		} else {
			fields["finished_at"] = nowUnix()
		}
		if perr := b.persistDecision(ctx, *tk, d, fields); perr != nil {
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		cleanup = append(cleanup, path.Join(dir, id))
		jobs[tk.JobID] = struct{}{}
	}

	// Job aggregates: FinishTask already refreshes when a Finisher is wired,
	// but the direct-write path (Finisher == nil) needs it, and a duplicate
	// refresh is a cheap idempotent DB write either way.
	for jobID := range jobs {
		if rerr := b.refreshJobStatus(ctx, jobID); rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}

	// Batch-delete settled markers in one shell call (rfs.FS has no Remove;
	// one exec beats one round trip per marker).
	if len(cleanup) > 0 {
		quoted := make([]string, len(cleanup))
		for i, p := range cleanup {
			quoted[i] = utils.ShellQuote(p)
		}
		if _, derr := b.shellRun(ctx, "rm -f "+strings.Join(quoted, " ")); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}

// HasInFlight reports whether this target currently has any non-terminal
// tasks with a live external id — the condition under which the daemon's
// marker-scan and probe-align loops run at all. Zero in-flight = zero SSH
// traffic (the design's silence contract).
func (b *Backend) HasInFlight(ctx context.Context) (bool, error) {
	jobs, err := b.Store.ListJobs(ctx, "", b.Cfg.Name)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Status != "done" {
			return true, nil
		}
	}
	return false, nil
}
