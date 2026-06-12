package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// JobRow maps to the `jobs` table in SQLite.
type JobRow struct {
	ID          string
	ProjectName string
	Description string
	Note        string
	ConfigJSON  string // serialized job.JobConfig (kept for UI to display original sweep config)
	Status      string // pending / running / paused / done
	TotalTasks  int
	CreatedAt   time.Time
	FinishedAt  *time.Time
	// RefreshedAt is the last time this job's state was reconciled from
	// external sources (HPC mode only — hpc.Refresh is its sole writer).
	// nil = never refreshed, or not applicable (daemon mode: the daemon IS
	// the source of truth, there is no reconcile concept).
	RefreshedAt *time.Time
	// ArchivedAt: archive = hide from default lists; data untouched. Lives
	// in its own column so config rewrites can never clobber it.
	ArchivedAt *time.Time
}

// TouchJobRefreshedAt records that a reconcile pass completed for this job.
func (s *Store) TouchJobRefreshedAt(ctx context.Context, jobID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE jobs SET refreshed_at = ? WHERE id = ?", at.Unix(), jobID)
	return err
}

// InsertJob inserts a single job row.
func (s *Store) InsertJob(ctx context.Context, j *JobRow) error {
	query := `INSERT INTO jobs (id, project_name, description, note, config_json, status, total_tasks, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query,
		j.ID, j.ProjectName, j.Description, j.Note, j.ConfigJSON,
		j.Status, j.TotalTasks, j.CreatedAt.Unix(),
	)
	return err
}

// InsertJobTx inserts a job row within an existing transaction.
func (s *Store) InsertJobTx(ctx context.Context, tx *sql.Tx, j *JobRow) error {
	query := `INSERT INTO jobs (id, project_name, description, note, config_json, status, total_tasks, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := tx.ExecContext(ctx, query,
		j.ID, j.ProjectName, j.Description, j.Note, j.ConfigJSON,
		j.Status, j.TotalTasks, j.CreatedAt.Unix(),
	)
	return err
}

// InsertJobWithTasks atomically inserts a job and all its tasks in a single transaction.
// If any step fails the entire batch is rolled back — no orphan job rows.
func (s *Store) InsertJobWithTasks(ctx context.Context, job *JobRow, tasks []TaskRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after successful Commit

	if err := s.InsertJobTx(ctx, tx, job); err != nil {
		return fmt.Errorf("insert job %s: %w", job.ID, err)
	}

	for i := range tasks {
		if err := s.InsertTaskTx(ctx, tx, &tasks[i]); err != nil {
			return fmt.Errorf("insert task %s: %w", tasks[i].ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// UpdateJobStatus updates a job's status.
// Automatically sets finished_at when transitioning to "done".
func (s *Store) UpdateJobStatus(ctx context.Context, jobID string, status string) error {
	var query string
	var args []any

	if status == "done" {
		query = "UPDATE jobs SET status = ?, finished_at = ? WHERE id = ?"
		args = []any{status, time.Now().Unix(), jobID}
	} else {
		query = "UPDATE jobs SET status = ? WHERE id = ?"
		args = []any{status, jobID}
	}

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %q not found", jobID)
	}
	return nil
}

// GetJob returns a single job by ID. Returns (nil, nil) if not found.
func (s *Store) GetJob(ctx context.Context, jobID string) (*JobRow, error) {
	query := `SELECT id, project_name, description, note, config_json, status, total_tasks, created_at, finished_at, refreshed_at, archived_at
		FROM jobs WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, jobID)

	j, err := scanJob(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// ListJobs lists jobs, optionally filtered by project name. Empty projectName = no filter.
func (s *Store) ListJobs(ctx context.Context, projectName string) ([]JobRow, error) {
	query := `SELECT id, project_name, description, note, config_json, status, total_tasks, created_at, finished_at, refreshed_at, archived_at
		FROM jobs`
	var args []any
	if projectName != "" {
		query += " WHERE project_name = ?"
		args = append(args, projectName)
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []JobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *j)
	}
	return result, rows.Err()
}

// DeleteJob removes a job and all its tasks from DB.
// Tasks are deleted first to satisfy the foreign key constraint.
func (s *Store) DeleteJob(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE job_id = ?", jobID); err != nil {
		return fmt.Errorf("delete tasks: %w", err)
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", jobID)
	if err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job %q not found", jobID)
	}
	return tx.Commit()
}

// scanJob reads one result row into a JobRow.
func scanJob(scanner interface{ Scan(dest ...any) error }) (*JobRow, error) {
	var j JobRow
	var (
		note        sql.NullString
		createdAt   int64
		finishedAt  sql.NullInt64
		refreshedAt sql.NullInt64
		archivedAt  sql.NullInt64
	)

	err := scanner.Scan(
		&j.ID, &j.ProjectName, &j.Description, &note, &j.ConfigJSON,
		&j.Status, &j.TotalTasks, &createdAt, &finishedAt, &refreshedAt, &archivedAt,
	)
	if err != nil {
		return nil, err
	}

	j.Note = note.String
	j.CreatedAt = time.Unix(createdAt, 0)
	j.FinishedAt = unixToNullTime(finishedAt)
	j.RefreshedAt = unixToNullTime(refreshedAt)
	j.ArchivedAt = unixToNullTime(archivedAt)
	return &j, nil
}

// ActiveStatuses is THE definition of "this job still has live work":
// queued, executing, or pause-resumable. Every guard that asks "is it safe
// to hide/forget this?" must use it — the paused-archivable inconsistency
// (GUI said no, CLI said yes) existed precisely because this list was
// inlined in three places. Mirrors the frontend's statusGrammar single-
// source philosophy (U3), backend edition.
var ActiveStatuses = []string{"pending", "running", "paused"}

// IsActiveStatus reports whether s is one of ActiveStatuses.
func IsActiveStatus(s string) bool {
	for _, a := range ActiveStatuses {
		if s == a {
			return true
		}
	}
	return false
}

// activeStatusesSQL renders ActiveStatuses as a SQL IN(...) literal — the
// values are compile-time constants, never user input.
func ActiveStatusesSQL() string {
	quoted := make([]string, len(ActiveStatuses))
	for i, s := range ActiveStatuses {
		quoted[i] = "'" + s + "'"
	}
	return "(" + strings.Join(quoted, ",") + ")"
}

// ── Archive ──
//
// Archive hides a job from default lists; nothing else changes (rows,
// workspace files and logs stay). Reversible via Unarchive. NOTE: ListJobs
// deliberately returns archived rows too (the {{version}} family scan must
// see every note ever used) — visibility filtering belongs to the listing
// methods below.

// ArchiveJob refuses active jobs: hiding something that is still holding
// GPUs/queue slots is how tasks get forgotten. Kill it first. paused counts
// as active — it is resumable, and a hidden-but-resumable job is the same
// forgotten-state hazard (matches the GUI, which only offers Archive on
// terminal jobs).
func (s *Store) ArchiveJob(ctx context.Context, jobID string) error {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("job %q not found", jobID)
	}
	if err != nil {
		return err
	}
	if IsActiveStatus(status) {
		return fmt.Errorf("job %s is %s — kill it (or let it finish) before archiving", jobID, status)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE jobs SET archived_at = ? WHERE id = ?`, time.Now().Unix(), jobID)
	return err
}

func (s *Store) UnarchiveJob(ctx context.Context, jobID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET archived_at = NULL WHERE id = ?`, jobID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("job %q not found", jobID)
	}
	return nil
}

// ListJobsVisible is the default listing: directly-archived jobs are hidden;
// with no project scope, jobs of archived projects are hidden too (cascade).
// Inside an explicit project scope the cascade does not apply — you navigated
// there on purpose.
func (s *Store) ListJobsVisible(ctx context.Context, projectName string) ([]JobRow, error) {
	query := `SELECT j.id, j.project_name, j.description, j.note, j.config_json, j.status, j.total_tasks, j.created_at, j.finished_at, j.refreshed_at, j.archived_at
		FROM jobs j`
	var args []any
	if projectName != "" {
		query += ` WHERE j.project_name = ? AND j.archived_at IS NULL`
		args = append(args, projectName)
	} else {
		query += ` JOIN projects p ON p.name = j.project_name
			WHERE j.archived_at IS NULL AND p.archived_at IS NULL`
	}
	query += " ORDER BY j.created_at DESC"
	return s.queryJobs(ctx, query, args...)
}

// ListJobsArchived returns the explicitly-archived jobs (cascade-hidden jobs
// of an archived project are NOT included — they come back with the project).
func (s *Store) ListJobsArchived(ctx context.Context, projectName string) ([]JobRow, error) {
	query := `SELECT id, project_name, description, note, config_json, status, total_tasks, created_at, finished_at, refreshed_at, archived_at
		FROM jobs WHERE archived_at IS NOT NULL`
	var args []any
	if projectName != "" {
		query += ` AND project_name = ?`
		args = append(args, projectName)
	}
	query += " ORDER BY created_at DESC"
	return s.queryJobs(ctx, query, args...)
}

func (s *Store) queryJobs(ctx context.Context, query string, args ...any) ([]JobRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []JobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *j)
	}
	return result, rows.Err()
}
