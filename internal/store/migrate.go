package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// Migrate executes schema.sql against the database, then runs idempotent
// ALTER TABLE steps for columns that were added to existing tables in later
// versions (CREATE TABLE IF NOT EXISTS does NOT add columns to pre-existing
// tables, so each new column needs explicit handling here).
//
// All steps are safe to run repeatedly.
func (s *Store) Migrate() error {
	ctx, cancel := defaultCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	if err := s.addMissingColumns(ctx); err != nil {
		return err
	}
	if err := s.archiveLegacyMetrics(ctx); err != nil {
		return err
	}
	return s.reclassifyDoneJobs(ctx)
}

// archiveLegacyMetrics preserves raw metric points created by releases before
// the streaming summary/pyramid design. Current code no longer reads or writes
// this table, but deleting it during startup would make an otherwise routine
// binary upgrade destructive. Renaming is an O(1) SQLite schema operation, so
// even databases with millions of historical points remain cheap to upgrade.
//
// New databases never create either table. If a prior/manual migration already
// created the archive, leave any additional `metrics` table untouched too: an
// ambiguous duplicate is safer than discarding user data.
func (s *Store) archiveLegacyMetrics(ctx context.Context) error {
	hasMetrics, err := tableExists(ctx, s.db, "metrics")
	if err != nil {
		return fmt.Errorf("inspect legacy metrics table: %w", err)
	}
	if !hasMetrics {
		return nil
	}
	hasArchive, err := tableExists(ctx, s.db, "metrics_legacy_v1")
	if err != nil {
		return fmt.Errorf("inspect legacy metrics archive: %w", err)
	}
	if hasArchive {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE metrics RENAME TO metrics_legacy_v1`); err != nil {
		return fmt.Errorf("archive legacy metrics: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var exists int
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND name = ?
		)`, name).Scan(&exists)
	return exists == 1, err
}

// reclassifyDoneJobs is the one-shot data migration for the terminal job
// status split (done → done/failed/partial/killed). Older versions
// collapsed every fully-terminal job into "done"; recompute the terminal
// status from each job's task outcomes using the same rules as
// TerminalJobStatus. Idempotent: after the first pass only all-success
// jobs still carry "done", and recomputing those yields "done" again.
// Guards: jobs with zero tasks keep "done" (nothing to aggregate), and
// jobs with ANY task outside the known-terminal vocabulary
// (success/failed/killed) are left untouched — that covers live states
// (pending/running) AND unknown/legacy statuses alike. A blocklist here
// would silently misclassify anything it didn't anticipate as
// failed/partial; only rows whose every task is positively terminal are
// safe to recompute (the live aggregator owns the rest).
func (s *Store) reclassifyDoneJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = (
			SELECT CASE
				WHEN SUM(t.status = 'success') = COUNT(*) THEN 'done'
				WHEN SUM(t.status = 'success') = 0 AND SUM(t.status = 'killed') = COUNT(*) THEN 'killed'
				WHEN SUM(t.status = 'success') = 0 THEN 'failed'
				ELSE 'partial'
			END
			FROM tasks t WHERE t.job_id = jobs.id
		)
		WHERE status = 'done'
		  AND EXISTS (SELECT 1 FROM tasks t WHERE t.job_id = jobs.id)
		  AND NOT EXISTS (
			SELECT 1 FROM tasks t
			WHERE t.job_id = jobs.id
			  AND t.status NOT IN ('success', 'failed', 'killed')
		  )`)
	if err != nil {
		return fmt.Errorf("reclassify done jobs: %w", err)
	}
	return nil
}

// addMissingColumns is the home for "tables already existed in an older schema,
// add this column" steps. Each addColumnIfMissing call is a no-op if the column
// is already present, so this is safe across daemon upgrades.
func (s *Store) addMissingColumns(ctx context.Context) error {
	// L2-C: task_dir holds the per-task workspace at <root>/<job_id>/<task_id>.
	// Without this, daemon restart can't locate the metrics.jsonl from reclaimed tasks.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "task_dir", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.task_dir: %w", err)
	}
	// RQ-75: lane-generation ownership. '' (pre-upgrade rows) is adopted by
	// the target's ACTIVE lane.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "target_generation", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("add tasks.target_generation: %w", err)
	}
	// Task execution timeouts predate the explicit migration list. Existing
	// databases whose tasks table was created before this column was added must
	// be upgraded before the normal task SELECT list can be queried.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "timeout", "INTEGER"); err != nil {
		return fmt.Errorf("add tasks.timeout: %w", err)
	}
	// L2-E: external_id holds the runqd attempt id or HPC scheduler job id so
	// refresh can map a task back to its execution record.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "external_id", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.external_id: %w", err)
	}
	// L2-E: status_source records where a task's status came from (wrapper /
	// scheduler / inferred / runq / submit / retry). Lets refresh treat "inferred"
	// terminals as correctable while hard terminals are final.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "status_source", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.status_source: %w", err)
	}
	// Durable K2 intent: a restart must not forget cancellation that raced a
	// remote submission or an external scheduler response.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "kill_requested", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("add tasks.kill_requested: %w", err)
	}
	// note: user-supplied experiment note (--note flag or job.yaml note: field).
	if err := addColumnIfMissing(ctx, s.db, "jobs", "note", "TEXT"); err != nil {
		return fmt.Errorf("add jobs.note: %w", err)
	}
	// orphaned_at: set when the task's task_dir has been confirmed missing
	// (rfs.FS-based detection with guardrails; see remote.DetectOrphans).
	// Reversible metadata — detection marks, only `runq clean --orphan`
	// deletes. NULL = not orphaned.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "orphaned_at", "INTEGER"); err != nil {
		return fmt.Errorf("add tasks.orphaned_at: %w", err)
	}
	// refreshed_at: last reconcile of job state from external sources.
	// HPC mode only (hpc.Refresh is the sole writer); always NULL in daemon
	// mode — the daemon is the source of truth, there is nothing to reconcile.
	if err := addColumnIfMissing(ctx, s.db, "jobs", "refreshed_at", "INTEGER"); err != nil {
		return fmt.Errorf("add jobs.refreshed_at: %w", err)
	}
	// archived_at: archive = hide from default lists, keep everything.
	// Lives in its own column (NOT config_json) so project.yaml sync and
	// job config rewrites can never clobber it.
	if err := addColumnIfMissing(ctx, s.db, "jobs", "archived_at", "INTEGER"); err != nil {
		return fmt.Errorf("add jobs.archived_at: %w", err)
	}
	if err := addColumnIfMissing(ctx, s.db, "projects", "archived_at", "INTEGER"); err != nil {
		return fmt.Errorf("add projects.archived_at: %w", err)
	}
	// Phase 1: target column for MultiBackend routing. Every job and task
	// records which compute target it was submitted to (e.g. "local",
	// "tsubame"). Defaults to "local" for pre-Phase-1 rows.
	if err := addColumnIfMissing(ctx, s.db, "jobs", "target", "TEXT NOT NULL DEFAULT 'local'"); err != nil {
		return fmt.Errorf("add jobs.target: %w", err)
	}
	if err := addColumnIfMissing(ctx, s.db, "tasks", "target", "TEXT NOT NULL DEFAULT 'local'"); err != nil {
		return fmt.Errorf("add tasks.target: %w", err)
	}
	// Phase 2D: HPC scheduler native state (e.g. Slurm CONFIGURING, COMPLETING).
	// Preserved from probe output before ParseSignal collapses it to a signal enum.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "native_state", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.native_state: %w", err)
	}
	// Phase 2D: scheduler queue/partition (Slurm partition, PBS/SGE queue).
	if err := addColumnIfMissing(ctx, s.db, "tasks", "queue", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.queue: %w", err)
	}
	// RQ-74: failure_detail carries the verbatim evidence for failures that
	// happen BEFORE the task runs (submit rejection: scheduler stderr + exit
	// code + rendered command). The run phase self-reports through logs and
	// status.json; this column extends the same honesty to the birth phase so
	// the death cause is visible in `runq task show` / the dashboard instead
	// of only in daemon logs.
	if err := addColumnIfMissing(ctx, s.db, "tasks", "failure_detail", "TEXT"); err != nil {
		return fmt.Errorf("add tasks.failure_detail: %w", err)
	}
	return nil
}

// addColumnIfMissing inspects the table via PRAGMA table_info and only runs
// ALTER TABLE ... ADD COLUMN when the column is absent. SQLite's ALTER TABLE
// only supports a small subset of changes; ADD COLUMN with a simple type is fine.
//
// Cannot use IF NOT EXISTS in ALTER TABLE prior to SQLite 3.35; the pragma
// pre-check works on every version.
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, colType string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctypeStr  string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctypeStr, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colType)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("%s: %w", stmt, err)
	}
	return nil
}
