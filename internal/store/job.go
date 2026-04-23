package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// JobRow maps to the `jobs` table in SQLite.
type JobRow struct {
	ID          string
	ProjectName string
	Description string
	ConfigJSON  string // serialized job.JobConfig (kept for UI to display original sweep config)
	Status      string // pending / running / paused / done
	TotalTasks  int
	CreatedAt   time.Time
	FinishedAt  *time.Time
}

// InsertJob inserts a single job row.
func (s *Store) InsertJob(ctx context.Context, j *JobRow) error {
	query := `INSERT INTO jobs (id, project_name, description, config_json, status, total_tasks, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query,
		j.ID, j.ProjectName, j.Description, j.ConfigJSON,
		j.Status, j.TotalTasks, j.CreatedAt.Unix(),
	)
	return err
}

// InsertJobTx inserts a job row within an existing transaction.
func (s *Store) InsertJobTx(ctx context.Context, tx *sql.Tx, j *JobRow) error {
	query := `INSERT INTO jobs (id, project_name, description, config_json, status, total_tasks, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := tx.ExecContext(ctx, query,
		j.ID, j.ProjectName, j.Description, j.ConfigJSON,
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
	query := `SELECT id, project_name, description, config_json, status, total_tasks, created_at, finished_at
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
	query := `SELECT id, project_name, description, config_json, status, total_tasks, created_at, finished_at
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
		createdAt  int64
		finishedAt sql.NullInt64
	)

	err := scanner.Scan(
		&j.ID, &j.ProjectName, &j.Description, &j.ConfigJSON,
		&j.Status, &j.TotalTasks, &createdAt, &finishedAt,
	)
	if err != nil {
		return nil, err
	}

	j.CreatedAt = time.Unix(createdAt, 0)
	j.FinishedAt = unixToNullTime(finishedAt)
	return &j, nil
}
