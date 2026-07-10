package backend

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// PerformClean removes tasks matching opts and cleans their on-disk artifacts.
// If opts.DryRun is true it returns a preview without deleting.
//
// fsFor resolves a task's TARGET to the filesystem its artifacts live on —
// remote tasks' dirs are on the remote (rm over Exec), local ones here.
// nil = everything is local (single-lane callers, tests).
func PerformClean(ctx context.Context, st *store.Store, fsFor func(target string) rfs.FS, opts CleanOptions) (*CleanResult, error) {
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

	// Build preview. Size stats come from the LEDGER (checkpoints table +
	// ingest marks) — zero filesystem/SSH touches at selection time.
	ids := make([]string, 0, len(unique))
	for _, u := range unique {
		ids = append(ids, u.task.ID)
	}
	stats, serr := st.TaskCleanStats(ctx, ids)
	if serr != nil {
		stats = map[string]store.TaskCleanStat{} // preview degrades, never blocks
	}
	resolveFS := func(target string) rfs.FS {
		if fsFor == nil {
			return nil // local semantics
		}
		return fsFor(target)
	}
	preview := make([]CleanPreviewItem, 0, len(unique))
	for _, u := range unique {
		action := classifyAction(u.task, resolveFS(u.task.Target))
		var finishedUnix *int64
		if u.task.FinishedAt != nil {
			ts := u.task.FinishedAt.Unix()
			finishedUnix = &ts
		}
		stat := stats[u.task.ID]
		preview = append(preview, CleanPreviewItem{
			TaskID:       u.task.ID,
			JobID:        u.task.JobID,
			Project:      u.task.ProjectName,
			Status:       u.task.Status,
			FinishedAt:   finishedUnix,
			TaskDir:      u.task.TaskDir,
			Reason:       u.reason,
			Action:       action,
			Orphan:       u.reason == "orphan",
			CkptFiles:    stat.CkptFiles,
			CkptBytes:    stat.CkptBytes,
			MetricsBytes: stat.MetricsBytes,
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
		// All-or-nothing by design: the DB record dies here, files die in
		// phase 2. Partial modes are gone — surgical cleanup is a cd away.
		if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", u.task.ID); err != nil {
			continue
		}
		deleted = append(deleted, dbDeleted{u.task, preview[i].Action})
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
	// Grouped by target: local dirs die via os.RemoveAll (exact freed
	// bytes); remote dirs die via ONE batched `rm -rf` per chunk over the
	// owning lane's Exec (freed bytes approximated from the ledger stats —
	// per-dir du over SSH isn't worth the round trips).
	var freedBytes int64
	remoteByTarget := map[string][]store.TaskRow{}
	for _, d := range deleted {
		if d.action != CleanActionAll {
			continue // db_only: nothing on disk
		}
		fsys := resolveFS(d.task.Target)
		if fsys != nil {
			if _, isLocal := fsys.(*rfs.LocalFS); !isLocal {
				remoteByTarget[d.task.Target] = append(remoteByTarget[d.task.Target], d.task)
				continue
			}
		}
		cleaned, cerr := cleanTaskArtifacts(d.task)
		if cerr != nil {
			slog.Warn("clean: failed to remove task artifacts",
				"task", d.task.ID, "err", cerr)
		}
		freedBytes += cleaned
	}
	for target, rows := range remoteByTarget {
		freedBytes += cleanRemoteArtifacts(ctx, resolveFS(target), rows, stats)
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
		// Only tasks already MARKED orphaned qualify (rfs.FS-based detection
		// with guardrails — see remote.DetectOrphans / LocalBackend.
		// DetectOrphansNow). Never stat paths here: this code runs on the
		// client, where a remote target's path is unobservable and a blind
		// os.Stat would misclassify every remote task as orphaned.
		rows, err := st.ListOrphanedTasks(ctx, opts.Target)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			if store.IsActiveStatus(r.Status) {
				continue
			}
			tasks = append(tasks, r)
			reasons = append(reasons, "orphan")
		}
	}

	// Exact-set selector: the execute phase of the interactive clean flow.
	for _, id := range opts.TaskIDs {
		tk, err := st.GetTask(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if tk != nil {
			tasks = append(tasks, *tk)
			reasons = append(reasons, "selected")
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

// classifyAction DESCRIBES what deletion will touch (all vs db_only) —
// deletion itself is always all-or-nothing.
//
// Local dirs are stat'ed (cheap, exact). REMOTE dirs are NOT: a per-task
// SSH stat at preview time would violate the zero-touch selection
// contract — a remote task with a dir is assumed deletable; rm -rf on an
// already-gone dir is a harmless no-op.
func classifyAction(t store.TaskRow, fsys rfs.FS) CleanAction {
	if t.TaskDir == "" {
		return CleanActionDBOnly
	}
	if fsys != nil {
		if _, isLocal := fsys.(*rfs.LocalFS); !isLocal {
			return CleanActionAll // remote: no stat at selection time
		}
	}
	if _, err := os.Stat(t.TaskDir); err != nil {
		return CleanActionDBOnly // dir missing — nothing to delete on disk
	}
	return CleanActionAll
}

// cleanRemoteArtifacts removes remote tasks' dirs + stray log files with
// batched `rm -rf --` over the lane's Exec (100 paths per invocation —
// command-line budget). Freed bytes are the LEDGER's estimate
// (checkpoints + metrics ingest size): honest enough for the summary
// line, and per-dir du over SSH would cost a round trip per task.
func cleanRemoteArtifacts(ctx context.Context, fsys rfs.FS, rows []store.TaskRow, stats map[string]store.TaskCleanStat) int64 {
	var paths []string
	var freed int64
	for _, t := range rows {
		// RQ-45: structural guard before anything reaches `rm -rf`.
		if t.TaskDir != "" {
			if err := utils.SafeDeletePath(t.TaskDir); err != nil {
				slog.Warn("clean: refusing suspicious path", "task", t.ID, "error", err)
			} else {
				paths = append(paths, t.TaskDir)
			}
		}
		if t.LogPath != "" && (t.TaskDir == "" || !strings.HasPrefix(t.LogPath, t.TaskDir)) {
			if err := utils.SafeDeletePath(t.LogPath); err != nil {
				slog.Warn("clean: refusing suspicious log path", "task", t.ID, "error", err)
			} else {
				paths = append(paths, t.LogPath)
			}
		}
		st := stats[t.ID]
		freed += st.CkptBytes + st.MetricsBytes
	}
	const batch = 100
	for start := 0; start < len(paths); start += batch {
		chunk := paths[start:min(start+batch, len(paths))]
		var sb strings.Builder
		sb.WriteString("rm -rf --")
		for _, p := range chunk {
			sb.WriteByte(' ')
			sb.WriteString(utils.ShellQuote(p))
		}
		if _, _, code, err := fsys.Exec(ctx, "sh", "-c", sb.String()); err != nil || code != 0 {
			slog.Warn("clean: remote artifact removal failed",
				"paths", len(chunk), "exit", code, "err", err)
		}
	}
	return freed
}

// cleanTaskArtifacts removes the task's workspace directory and log file.
// Returns freed bytes and the first error encountered (if any).
func cleanTaskArtifacts(t store.TaskRow) (int64, error) {
	var freed int64
	var firstErr error
	if t.TaskDir != "" {
		if err := utils.SafeDeletePath(t.TaskDir); err != nil {
			firstErr = err
		} else {
			size := dirSize(t.TaskDir)
			if err := os.RemoveAll(t.TaskDir); err != nil {
				firstErr = err
			} else {
				freed += size
			}
		}
	}
	if t.LogPath != "" && (t.TaskDir == "" || !strings.HasPrefix(t.LogPath, t.TaskDir)) {
		if err := utils.SafeDeletePath(t.LogPath); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else if info, err := os.Stat(t.LogPath); err == nil {
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
