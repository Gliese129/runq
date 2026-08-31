package remote

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/utils"
)

// ErrMarkerDetectionDisabled means no marker source is configured, so no
// remote observation was attempted. ScanDoneMarkers returns nil only after a
// configured marker source has been observed successfully.
var ErrMarkerDetectionDisabled = errors.New("done marker detection is disabled")

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
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	dir := b.doneDir()
	if dir == "" {
		return ErrMarkerDetectionDisabled
	}
	entries, err := b.FS.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		b.recordContact(nil) // dir absent = successful contact, nothing finished yet
		return nil
	}
	b.recordContact(err) // the 2min marker scan is /health's main heartbeat
	if err != nil {
		return fmt.Errorf("scan done markers: %w", err)
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
		if gerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("look up completion marker owner %s: %w", id, gerr)
			}
			continue
		}
		if tk == nil || tk.Target != b.Cfg.Name {
			continue // not ours — leave the marker alone
		}
		// Generation ownership (review round 3): with a shared workspace,
		// active and retiring lanes see the SAME done dir — only the
		// owning lane may read, settle and delete a task's marker.
		// (Adoption restamps rows at restore, so exact match suffices.)
		if b.Scope != nil && tk.TargetGeneration != b.Scope.Generation {
			continue
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

		sf, statusErr := readStatusObserved(b.FS, tk.TaskDir)
		if statusErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("read completion marker status for %s: %w", tk.ID, statusErr)
			}
			continue
		}
		if sf.Status == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("completion marker for %s has no valid status yet", tk.ID)
			}
			continue
		}
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

// HasInFlight reports whether this lane owns work that may already exist
// outside runq-lab. Generation scope is load-bearing here: an empty active
// lane must not stay awake merely because a retiring lane owns the target's
// remaining work, and vice versa.
func (b *Backend) HasInFlight(ctx context.Context) (bool, error) {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{Target: b.Cfg.Name, Scope: b.Scope})
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		if task.ExternalID != "" || task.Status == "submitting" || task.Status == "running" || task.Status == "unknown" {
			return true, nil
		}
	}
	return false, nil
}
