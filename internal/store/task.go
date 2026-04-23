package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// TaskRow maps to the `tasks` table in SQLite.
// Unlike scheduler.Task (an in-memory scheduling object with runtime fields like Env map),
// TaskRow is a pure data-transfer object whose field types mirror the DB schema.
// Callers are responsible for converting between the two.
type TaskRow struct {
	ID          string
	JobID       string
	ProjectName string
	Command     string
	ParamsJSON  string // JSON-serialized parameter map
	GPUsNeeded  int
	GPUs        string // comma-separated GPU indices, e.g. "0,1,3"
	Status      string
	RetryCount  int
	MaxRetry    int
	PID         int
	StartTime   int64 // /proc starttime (Unix timestamp) for reclaim validation
	LogPath     string
	WorkingDir  string
	EnvJSON     string // JSON-serialized environment variable map
	Resumable   bool
	ExtraArgs   string
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

// TaskFilter holds optional filter criteria for ListTasks.
// Zero values mean "no filter".
type TaskFilter struct {
	Status string // filter by status; empty = no filter
	JobID  string // filter by job; empty = no filter
}

// allTaskColumns lists every column in the tasks table.
// Defined once so SELECT and Scan stay in sync; adding a column means editing one place.
const allTaskColumns = `id, job_id, project_name, command, params_json,
	gpus_needed, gpus, status, retry_count, max_retry,
	pid, start_time, log_path, working_dir, env_json,
	resumable, extra_args, enqueued_at, started_at, finished_at`

// scanTask reads one result row into a TaskRow.
// Column order must match allTaskColumns.
func scanTask(scanner interface{ Scan(dest ...any) error }) (*TaskRow, error) {
	var t TaskRow
	var (
		gpus       sql.NullString
		pid        sql.NullInt64
		startTime  sql.NullInt64
		logPath    sql.NullString
		workingDir sql.NullString
		envJSON    sql.NullString
		resumable  int
		extraArgs  sql.NullString
		enqueuedAt int64
		startedAt  sql.NullInt64
		finishedAt sql.NullInt64
	)

	err := scanner.Scan(
		&t.ID, &t.JobID, &t.ProjectName, &t.Command, &t.ParamsJSON,
		&t.GPUsNeeded, &gpus, &t.Status, &t.RetryCount, &t.MaxRetry,
		&pid, &startTime, &logPath, &workingDir, &envJSON,
		&resumable, &extraArgs, &enqueuedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		return nil, err
	}

	t.GPUs = gpus.String
	t.PID = int(pid.Int64)
	t.StartTime = startTime.Int64
	t.LogPath = logPath.String
	t.WorkingDir = workingDir.String
	t.EnvJSON = envJSON.String
	t.Resumable = resumable != 0
	t.ExtraArgs = extraArgs.String
	t.EnqueuedAt = time.Unix(enqueuedAt, 0)
	t.StartedAt = unixToNullTime(startedAt)
	t.FinishedAt = unixToNullTime(finishedAt)

	return &t, nil
}

// InsertTask inserts a single task row.
func (s *Store) InsertTask(ctx context.Context, t *TaskRow) error {
	query := `INSERT INTO tasks (
		id, job_id, project_name, command, params_json,
		gpus_needed, gpus, status, retry_count, max_retry,
		pid, start_time, log_path, working_dir, env_json,
		resumable, extra_args, enqueued_at, started_at, finished_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	resumable := 0
	if t.Resumable {
		resumable = 1
	}

	_, err := s.db.ExecContext(ctx, query,
		t.ID, t.JobID, t.ProjectName, t.Command, t.ParamsJSON,
		t.GPUsNeeded, nullString(t.GPUs), t.Status, t.RetryCount, t.MaxRetry,
		nullInt(t.PID), nullInt64(t.StartTime), nullString(t.LogPath),
		nullString(t.WorkingDir), nullString(t.EnvJSON),
		resumable, t.ExtraArgs, t.EnqueuedAt.Unix(),
		nullTimeToUnix(t.StartedAt), nullTimeToUnix(t.FinishedAt),
	)
	return err
}

// InsertTaskTx inserts a task row within an existing transaction (used by InsertJobWithTasks).
func (s *Store) InsertTaskTx(ctx context.Context, tx *sql.Tx, t *TaskRow) error {
	query := `INSERT INTO tasks (
		id, job_id, project_name, command, params_json,
		gpus_needed, gpus, status, retry_count, max_retry,
		pid, start_time, log_path, working_dir, env_json,
		resumable, extra_args, enqueued_at, started_at, finished_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	resumable := 0
	if t.Resumable {
		resumable = 1
	}

	_, err := tx.ExecContext(ctx, query,
		t.ID, t.JobID, t.ProjectName, t.Command, t.ParamsJSON,
		t.GPUsNeeded, nullString(t.GPUs), t.Status, t.RetryCount, t.MaxRetry,
		nullInt(t.PID), nullInt64(t.StartTime), nullString(t.LogPath),
		nullString(t.WorkingDir), nullString(t.EnvJSON),
		resumable, t.ExtraArgs, t.EnqueuedAt.Unix(),
		nullTimeToUnix(t.StartedAt), nullTimeToUnix(t.FinishedAt),
	)
	return err
}

// UpdateTaskStatus updates a task's status and any extra fields.
//
// fields is a map of column name → new value for additional columns to update.
// Example:
//
//	store.UpdateTaskStatus(ctx, id, "running", map[string]any{
//	    "pid": 12345, "gpus": "0,1", "started_at": time.Now().Unix(),
//	})
func (s *Store) UpdateTaskStatus(ctx context.Context, taskID string, status string, fields map[string]any) error {
	setClauses := []string{"status = ?"}
	args := []any{status}

	for col, val := range fields {
		setClauses = append(setClauses, col+" = ?")
		args = append(args, val)
	}

	query := fmt.Sprintf("UPDATE tasks SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	args = append(args, taskID)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("task %q not found", taskID)
	}
	return nil
}

// GetTask returns a single task by ID. Returns (nil, nil) if not found.
func (s *Store) GetTask(ctx context.Context, taskID string) (*TaskRow, error) {
	query := fmt.Sprintf("SELECT %s FROM tasks WHERE id = ?", allTaskColumns)
	row := s.db.QueryRowContext(ctx, query, taskID)

	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListTasks queries tasks with optional status/job filters.
func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]TaskRow, error) {
	var where []string
	var args []any

	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.JobID != "" {
		where = append(where, "job_id = ?")
		args = append(args, filter.JobID)
	}

	query := fmt.Sprintf("SELECT %s FROM tasks", allTaskColumns)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY enqueued_at ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TaskRow
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, rows.Err()
}

// ListActiveTasks returns all pending or running tasks, ordered by enqueue time.
// Called at daemon startup to rebuild the in-memory Queue.
func (s *Store) ListActiveTasks(ctx context.Context) ([]TaskRow, error) {
	query := fmt.Sprintf(
		"SELECT %s FROM tasks WHERE status IN ('pending', 'running') ORDER BY enqueued_at ASC",
		allTaskColumns,
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TaskRow
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *t)
	}
	return result, rows.Err()
}

// ── Helpers ──

// nullTimeToUnix converts *time.Time to sql.NullInt64 for DB writes.
func nullTimeToUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// unixToNullTime converts sql.NullInt64 back to *time.Time for DB reads.
func unixToNullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0)
	return &t
}

// nullString converts an empty string to sql.NullString (stored as NULL rather than "").
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullInt converts 0 to sql.NullInt64 (PID=0 means not started → store NULL).
func nullInt(n int) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

// nullInt64 converts 0 to sql.NullInt64.
func nullInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}
