package backend

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gliese129/runq/internal/store"
)

// PerformClean removes tasks matching opts and cleans their on-disk artifacts.
// If opts.DryRun is true it returns a preview without deleting.
// Shared by HPCBackend.Clean (in-process) and the daemon API handler (over HTTP).
func PerformClean(ctx context.Context, st *store.Store, opts CleanOptions) (*CleanResult, error) {
	tasks, reason, err := collectCleanTargets(ctx, st, opts)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}

	// Deduplicate (selectors may overlap).
	seen := make(map[string]bool)
	type tagged struct {
		task   store.TaskRow
		reason string
	}
	var unique []tagged
	for i, t := range tasks {
		if !seen[t.ID] {
			seen[t.ID] = true
			unique = append(unique, tagged{t, reason[i]})
		}
	}

	// Exclude active tasks — never clean running/pending/paused work.
	{
		var safe []tagged
		for _, u := range unique {
			if store.IsActiveStatus(u.task.Status) {
				continue
			}
			safe = append(safe, u)
		}
		unique = safe
	}

	// Apply --older-than as an additional filter when combined with selectors.
	if opts.OlderThan != nil {
		cutoff := *opts.OlderThan
		var filtered []tagged
		for _, u := range unique {
			if u.task.FinishedAt != nil && u.task.FinishedAt.Before(cutoff) {
				filtered = append(filtered, u)
			}
			// Tasks without finished_at have unknown age and are excluded.
		}
		unique = filtered
	}

	// Build preview.
	preview := make([]CleanPreviewItem, 0, len(unique))
	for _, u := range unique {
		action := classifyAction(u.task, opts.CkptOnly)
		var finishedUnix *int64
		if u.task.FinishedAt != nil {
			ts := u.task.FinishedAt.Unix()
			finishedUnix = &ts
		}
		preview = append(preview, CleanPreviewItem{
			TaskID:     u.task.ID,
			JobID:      u.task.JobID,
			Status:     u.task.Status,
			FinishedAt: finishedUnix,
			TaskDir:    u.task.TaskDir,
			Reason:     u.reason,
			Action:     action,
			Orphan:     u.reason == "orphan",
		})
	}

	if opts.DryRun {
		return &CleanResult{Preview: preview}, nil
	}

	// ── Phase 1: DB mutations inside a transaction ──
	// All DB deletes happen first. File deletion is deferred until after
	// commit so a rollback never leaves orphaned DB rows pointing at
	// already-deleted workspaces.
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin clean tx: %w", err)
	}
	defer tx.Rollback()

	// Track which tasks had their DB row deleted so we can clean files
	// in phase 2.
	type dbDeleted struct {
		task   store.TaskRow
		action CleanAction
	}
	var deleted []dbDeleted

	for i, u := range unique {
		action := preview[i].Action
		switch action {
		case CleanActionAll, CleanActionDBOnly:
			if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", u.task.ID); err != nil {
				continue
			}
			deleted = append(deleted, dbDeleted{u.task, action})
		case CleanActionCkpt:
			// Checkpoint-only: no DB mutation, just file deletion in phase 2.
			deleted = append(deleted, dbDeleted{u.task, action})
		case CleanActionCkptDB:
			continue
		}
	}

	// Delete orphan jobs (done jobs with no remaining tasks) within the same tx.
	// The status='done' guard mirrors store.DeleteOrphanJobs — never remove
	// pending/running/paused jobs that just haven't spawned tasks yet.
	// When a target filter is active, only delete orphan jobs matching that
	// target so `clean --target X` can't accidentally remove another target's
	// empty done jobs.
	var res sql.Result
	if opts.Target != "" {
		res, _ = tx.ExecContext(ctx,
			"DELETE FROM jobs WHERE status = 'done' AND target = ? AND id NOT IN (SELECT DISTINCT job_id FROM tasks)",
			opts.Target)
	} else {
		res, _ = tx.ExecContext(ctx,
			"DELETE FROM jobs WHERE status = 'done' AND id NOT IN (SELECT DISTINCT job_id FROM tasks)")
	}
	deletedJobs := int64(0)
	if res != nil {
		deletedJobs, _ = res.RowsAffected()
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit clean tx: %w", err)
	}

	// ── Phase 2: file deletion (after successful commit) ──
	var freedBytes int64
	for _, d := range deleted {
		switch d.action {
		case CleanActionAll:
			cleaned, cerr := cleanTaskArtifacts(d.task)
			if cerr != nil {
				slog.Warn("clean: failed to remove task artifacts",
					"task", d.task.ID, "err", cerr)
			}
			freedBytes += cleaned
		case CleanActionCkpt:
			ckptDir := filepath.Join(d.task.TaskDir, "checkpoints")
			freedBytes += dirSize(ckptDir)
			if err := os.RemoveAll(ckptDir); err != nil {
				slog.Warn("clean: failed to remove checkpoints",
					"task", d.task.ID, "err", err)
			}
		}
	}

	return &CleanResult{
		Tasks:      len(deleted),
		Jobs:       int(deletedJobs),
		FreedBytes: freedBytes,
	}, nil
}

// collectCleanTargets gathers tasks matching the opts selectors.
// Returns parallel slices of tasks and their selection reasons.
func collectCleanTargets(ctx context.Context, st *store.Store, opts CleanOptions) ([]store.TaskRow, []string, error) {
	var tasks []store.TaskRow
	var reasons []string

	if opts.Orphan {
		// On-demand orphan detection: query non-active tasks with a task_dir,
		// then os.Stat each to find missing directories.
		rows, err := st.ListTasks(ctx, store.TaskFilter{})
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			if store.IsActiveStatus(r.Status) || r.TaskDir == "" {
				continue
			}
			if _, serr := os.Stat(r.TaskDir); serr != nil && os.IsNotExist(serr) {
				tasks = append(tasks, r)
				reasons = append(reasons, "orphan")
			}
		}
	}

	if opts.Archived {
		rows, err := st.ListArchivedTasks(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			tasks = append(tasks, r)
			reasons = append(reasons, "archived")
		}
	}

	if opts.JobID != "" {
		rows, err := st.ListTasks(ctx, store.TaskFilter{JobID: opts.JobID})
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			tasks = append(tasks, r)
			reasons = append(reasons, "job")
		}
	}

	if opts.TaskID != "" {
		t, err := st.GetTask(ctx, opts.TaskID)
		if err != nil {
			return nil, nil, err
		}
		if t != nil {
			tasks = append(tasks, *t)
			reasons = append(reasons, "task")
		}
	}

	// When only --older-than is given (no other selector), fall back to
	// finished-before-cutoff query — this preserves the original behavior.
	if !opts.Orphan && !opts.Archived && opts.JobID == "" && opts.TaskID == "" && opts.OlderThan != nil {
		rows, err := st.ListFinishedTasksBefore(ctx, *opts.OlderThan)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			tasks = append(tasks, r)
			reasons = append(reasons, "older-than")
		}
		// Clear OlderThan so the caller doesn't double-filter.
		opts.OlderThan = nil
	}

	// Filter by target when specified — only keep tasks belonging to the
	// requested compute target so `clean --target X` cannot accidentally
	// delete tasks from other targets.
	if opts.Target != "" {
		var filtered []store.TaskRow
		var filteredReasons []string
		for i, t := range tasks {
			if t.Target == opts.Target {
				filtered = append(filtered, t)
				filteredReasons = append(filteredReasons, reasons[i])
			}
		}
		tasks = filtered
		reasons = filteredReasons
	}

	return tasks, reasons, nil
}

// classifyAction determines what the clean operation will do for a task.
func classifyAction(t store.TaskRow, ckptOnly bool) CleanAction {
	if ckptOnly {
		if t.TaskDir == "" {
			return CleanActionCkptDB // no dir, nothing to delete
		}
		ckptDir := filepath.Join(t.TaskDir, "checkpoints")
		if info, err := os.Stat(ckptDir); err != nil || !info.IsDir() {
			return CleanActionCkptDB
		}
		return CleanActionCkpt
	}
	if t.TaskDir == "" {
		return CleanActionDBOnly
	}
	if _, err := os.Stat(t.TaskDir); err != nil {
		return CleanActionDBOnly // dir missing — nothing to delete on disk
	}
	return CleanActionAll
}

// cleanTaskArtifacts removes the task's workspace directory and log file.
// Returns freed bytes and the first error encountered (if any).
func cleanTaskArtifacts(t store.TaskRow) (int64, error) {
	var freed int64
	var firstErr error
	if t.TaskDir != "" {
		size := dirSize(t.TaskDir)
		if err := os.RemoveAll(t.TaskDir); err != nil {
			firstErr = err
		} else {
			freed += size
		}
	}
	if t.LogPath != "" && (t.TaskDir == "" || !strings.HasPrefix(t.LogPath, t.TaskDir)) {
		if info, err := os.Stat(t.LogPath); err == nil {
			logSize := info.Size()
			if err := os.Remove(t.LogPath); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				freed += logSize
			}
		}
	}
	return freed, firstErr
}

func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
