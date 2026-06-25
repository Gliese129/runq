package backend

import (
	"context"
	"fmt"
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

	// Apply --older-than as an additional filter when combined with selectors.
	if opts.OlderThan != nil {
		cutoff := *opts.OlderThan
		var filtered []tagged
		for _, u := range unique {
			if u.task.FinishedAt != nil && u.task.FinishedAt.Before(cutoff) {
				filtered = append(filtered, u)
			} else if u.task.FinishedAt == nil && u.task.OrphanAt != nil && u.task.OrphanAt.Before(cutoff) {
				// No finished_at — use orphan_at as age proxy.
				filtered = append(filtered, u)
			}
			// Tasks with neither finished_at nor orphan_at before cutoff
			// have unknown age and are excluded.
		}
		unique = filtered
	}

	// Build preview.
	preview := make([]CleanPreviewItem, 0, len(unique))
	for _, u := range unique {
		action := classifyAction(u.task, opts.CkptOnly)
		preview = append(preview, CleanPreviewItem{
			TaskID:     u.task.ID,
			JobID:      u.task.JobID,
			Status:     u.task.Status,
			FinishedAt: u.task.FinishedAt,
			TaskDir:    u.task.TaskDir,
			Reason:     u.reason,
			Action:     action,
			Orphan:     u.task.OrphanAt != nil,
		})
	}

	if opts.DryRun {
		return &CleanResult{Preview: preview}, nil
	}

	var deletedTasks int
	var freedBytes int64
	for i, u := range unique {
		action := preview[i].Action
		switch action {
		case CleanActionAll:
			freedBytes += cleanTaskArtifacts(u.task)
			if err := st.DeleteTask(ctx, u.task.ID); err != nil {
				continue
			}
		case CleanActionDBOnly:
			if err := st.DeleteTask(ctx, u.task.ID); err != nil {
				continue
			}
		case CleanActionCkpt:
			ckptDir := filepath.Join(u.task.TaskDir, "checkpoints")
			freedBytes += dirSize(ckptDir)
			os.RemoveAll(ckptDir)
		case CleanActionCkptDB:
			// ckpt-only requested but no checkpoints dir — skip.
			continue
		}
		deletedTasks++
	}

	deletedJobs, _ := st.DeleteOrphanJobs(ctx)

	return &CleanResult{
		Tasks:      deletedTasks,
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
		rows, err := st.ListOrphanTasks(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			tasks = append(tasks, r)
			reasons = append(reasons, "orphan")
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
	if t.OrphanAt != nil || t.TaskDir == "" {
		return CleanActionDBOnly
	}
	return CleanActionAll
}

// cleanTaskArtifacts removes the task's workspace directory and log file.
func cleanTaskArtifacts(t store.TaskRow) int64 {
	var freed int64
	if t.TaskDir != "" {
		freed += dirSize(t.TaskDir)
		os.RemoveAll(t.TaskDir)
	}
	if t.LogPath != "" && (t.TaskDir == "" || !strings.HasPrefix(t.LogPath, t.TaskDir)) {
		if info, err := os.Stat(t.LogPath); err == nil {
			freed += info.Size()
			os.Remove(t.LogPath)
		}
	}
	return freed
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
