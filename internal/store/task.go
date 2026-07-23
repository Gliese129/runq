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
	UID         int // submitting user's UID
	Timeout     int // timeout in seconds, 0 = no timeout
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time

	// L2-C: per-task workspace at <root>/<job_id>/<task_id>.
	// Holds params.json, wandb_config.json (optional), metrics.jsonl, checkpoints/.
	// Created by service.JobService.SubmitJob, read by SDK via RUNQ_TASK_DIR env.
	TaskDir string

	// Target is the compute target this task was submitted to (e.g. "local",
	// "tsubame"). Phase 1: MultiBackend routing key.
	Target string

	// L2-E: HPC scheduler job id (sbatch/qsub). Empty for daemon-managed tasks.
	// Set by the HPC backend after submit; used by refresh to map a task back to
	// its cluster job for status/kill.
	ExternalID string

	// L2-E: provenance of Status — "" | wrapper | scheduler | inferred | runq |
	// submit. Lets HPC refresh treat "inferred" terminals as correctable while
	// wrapper/scheduler/runq terminals are final. Empty for daemon tasks.
	StatusSource string

	// RQ-74: verbatim failure evidence for pre-run failures (submit rejection:
	// scheduler stderr + exit code + rendered command; local spawn errors).
	// Empty for tasks that reached the run phase — those self-report through
	// logs/status.json. Cleared on requeue (a new attempt starts clean).
	FailureDetail string

	// Phase 2D: scheduler-native state token (e.g. "CONFIGURING", "COMPLETING").
	// Written by the probe layer before ParseSignal collapses it. Empty for
	// daemon tasks or when no probe has run yet.
	NativeState string
	// Phase 2D: scheduler queue (Slurm partition, PBS/SGE queue). Captured
	// from probe output when available.
	Queue string

	// RQ-75: semantic hash of the target config generation that OWNS this
	// task. Kill/log/refresh route to the owning lane (a retiring old
	// generation keeps tracking its tasks on the old endpoint). '' = legacy
	// row, adopted by the active lane.
	TargetGeneration string
}

// TaskFilter holds optional filter criteria for ListTasks.
// Zero values mean "no filter".
type TaskFilter struct {
	Status string // filter by status; empty = no filter
	JobID  string // filter by job; empty = no filter
	Target string // filter by target; empty = no filter
	Limit  int    // page size; 0 = no limit (D20: SQL-level pagination)
	Offset int    // page start; only applied when Limit > 0
}

// allTaskColumns lists every column in the tasks table.
// Defined once so SELECT and Scan stay in sync; adding a column means editing one place.
const allTaskColumns = `id, job_id, project_name, command, params_json,
	gpus_needed, gpus, status, retry_count, max_retry,
	pid, start_time, log_path, working_dir, env_json,
	resumable, extra_args, uid, timeout,
	enqueued_at, started_at, finished_at, task_dir, target, external_id, status_source,
	native_state, queue, failure_detail, target_generation`

// scanTask reads one result row into a TaskRow.
// Column order must match allTaskColumns.
func scanTask(scanner interface{ Scan(dest ...any) error }) (*TaskRow, error) {
	var t TaskRow
	var (
		gpus          sql.NullString
		pid           sql.NullInt64
		startTime     sql.NullInt64
		logPath       sql.NullString
		workingDir    sql.NullString
		envJSON       sql.NullString
		resumable     int
		extraArgs     sql.NullString
		uid           sql.NullInt64
		timeout       sql.NullInt64
		enqueuedAt    int64
		startedAt     sql.NullInt64
		finishedAt    sql.NullInt64
		taskDir       sql.NullString
		externalID    sql.NullString
		statusSource  sql.NullString
		nativeState   sql.NullString
		queue         sql.NullString
		failureDetail sql.NullString
		targetGen     sql.NullString
	)

	var target sql.NullString
	err := scanner.Scan(
		&t.ID, &t.JobID, &t.ProjectName, &t.Command, &t.ParamsJSON,
		&t.GPUsNeeded, &gpus, &t.Status, &t.RetryCount, &t.MaxRetry,
		&pid, &startTime, &logPath, &workingDir, &envJSON,
		&resumable, &extraArgs, &uid, &timeout, &enqueuedAt, &startedAt, &finishedAt,
		&taskDir, &target, &externalID, &statusSource,
		&nativeState, &queue, &failureDetail, &targetGen,
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
	t.UID = int(uid.Int64)
	t.Timeout = int(timeout.Int64)
	t.EnqueuedAt = time.Unix(enqueuedAt, 0)
	t.StartedAt = unixToNullTime(startedAt)
	t.FinishedAt = unixToNullTime(finishedAt)
	t.TaskDir = taskDir.String
	t.Target = target.String
	if t.Target == "" {
		t.Target = "local"
	}
	t.ExternalID = externalID.String
	t.StatusSource = statusSource.String
	t.NativeState = nativeState.String
	t.Queue = queue.String
	t.FailureDetail = failureDetail.String
	t.TargetGeneration = targetGen.String

	return &t, nil
}

// InsertTask inserts a single task row.
func (s *Store) InsertTask(ctx context.Context, t *TaskRow) error {
	query := `INSERT INTO tasks (
		id, job_id, project_name, command, params_json,
		gpus_needed, gpus, status, retry_count, max_retry,
		pid, start_time, log_path, working_dir, env_json,
		resumable, extra_args, uid, timeout,
		enqueued_at, started_at, finished_at, task_dir, target, external_id, status_source,
		native_state, queue, failure_detail, target_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	resumable := 0
	if t.Resumable {
		resumable = 1
	}

	_, err := s.db.ExecContext(ctx, query,
		t.ID, t.JobID, t.ProjectName, t.Command, t.ParamsJSON,
		t.GPUsNeeded, nullString(t.GPUs), t.Status, t.RetryCount, t.MaxRetry,
		nullInt(t.PID), nullInt64(t.StartTime), nullString(t.LogPath),
		nullString(t.WorkingDir), nullString(t.EnvJSON),
		resumable, t.ExtraArgs, nullInt(t.UID), nullInt(t.Timeout),
		t.EnqueuedAt.Unix(),
		nullTimeToUnix(t.StartedAt), nullTimeToUnix(t.FinishedAt),
		nullString(t.TaskDir), targetOrDefault(t.Target), nullString(t.ExternalID), nullString(t.StatusSource),
		nullString(t.NativeState), nullString(t.Queue), nullString(t.FailureDetail),
		t.TargetGeneration,
	)
	return err
}

// InsertTaskTx inserts a task row within an existing transaction (used by InsertJobWithTasks).
func (s *Store) InsertTaskTx(ctx context.Context, tx *sql.Tx, t *TaskRow) error {
	query := `INSERT INTO tasks (
		id, job_id, project_name, command, params_json,
		gpus_needed, gpus, status, retry_count, max_retry,
		pid, start_time, log_path, working_dir, env_json,
		resumable, extra_args, uid, timeout,
		enqueued_at, started_at, finished_at, task_dir, target, external_id, status_source,
		native_state, queue, failure_detail, target_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	resumable := 0
	if t.Resumable {
		resumable = 1
	}

	_, err := tx.ExecContext(ctx, query,
		t.ID, t.JobID, t.ProjectName, t.Command, t.ParamsJSON,
		t.GPUsNeeded, nullString(t.GPUs), t.Status, t.RetryCount, t.MaxRetry,
		nullInt(t.PID), nullInt64(t.StartTime), nullString(t.LogPath),
		nullString(t.WorkingDir), nullString(t.EnvJSON),
		resumable, t.ExtraArgs, nullInt(t.UID), nullInt(t.Timeout),
		t.EnqueuedAt.Unix(),
		nullTimeToUnix(t.StartedAt), nullTimeToUnix(t.FinishedAt),
		nullString(t.TaskDir), targetOrDefault(t.Target), nullString(t.ExternalID), nullString(t.StatusSource),
		nullString(t.NativeState), nullString(t.Queue), nullString(t.FailureDetail),
		t.TargetGeneration,
	)
	return err
}

// allowedStatusFields is the whitelist of column names that UpdateTaskStatus
// accepts. Any key not in this set is rejected to prevent accidental SQL
// injection from future callers.
var allowedStatusFields = map[string]bool{
	"pid":            true,
	"gpus":           true,
	"start_time":     true,
	"started_at":     true,
	"finished_at":    true,
	"retry_count":    true,
	"log_path":       true,
	"working_dir":    true,
	"env_json":       true,
	"external_id":    true,
	"status_source":  true,
	"extra_args":     true,
	"native_state":   true,
	"queue":          true,
	"failure_detail": true,
}

// UpdateTaskStatus updates a task's status and any extra fields.
//
// fields is a map of column name → new value for additional columns to update.
// Only columns listed in allowedStatusFields are accepted; unknown keys return
// an error.
//
// Example:
//
//	store.UpdateTaskStatus(ctx, id, "running", map[string]any{
//	    "pid": 12345, "gpus": "0,1", "started_at": time.Now().Unix(),
//	})
func (s *Store) UpdateTaskStatus(ctx context.Context, taskID string, status string, fields map[string]any) error {
	setClauses := []string{"status = ?"}
	args := []any{status}

	for col, val := range fields {
		if !allowedStatusFields[col] {
			return fmt.Errorf("UpdateTaskStatus: column %q not in whitelist", col)
		}
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

// ── Orphan marking ─────────────────────────────────────────────────────────
//
// A task is "orphaned" when its task_dir has been confirmed missing (deleted
// by the user, or purged by a cluster scratch policy). Marking is REVERSIBLE
// metadata — detection marks, only an explicit `runq clean --orphan` deletes.

// MarkTaskOrphaned stamps orphaned_at (idempotent: keeps the first stamp).
func (s *Store) MarkTaskOrphaned(ctx context.Context, taskID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE tasks SET orphaned_at = ? WHERE id = ? AND orphaned_at IS NULL",
		at.Unix(), taskID)
	return err
}

// ClearTaskOrphaned removes the orphan mark (the directory reappeared —
// e.g. restored from backup, or the earlier observation was wrong).
func (s *Store) ClearTaskOrphaned(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE tasks SET orphaned_at = NULL WHERE id = ?", taskID)
	return err
}

// ListOrphanedTasks returns tasks currently marked orphaned, optionally
// scoped to one target.
func (s *Store) ListOrphanedTasks(ctx context.Context, target string) ([]TaskRow, error) {
	query := fmt.Sprintf("SELECT %s FROM tasks WHERE orphaned_at IS NOT NULL", allTaskColumns)
	var args []any
	if target != "" {
		query += " AND target = ?"
		args = append(args, target)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TaskRow
	for rows.Next() {
		t, serr := scanTask(rows)
		if serr != nil {
			return nil, serr
		}
		result = append(result, *t)
	}
	return result, rows.Err()
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
	if filter.Target != "" {
		where = append(where, "target = ?")
		args = append(args, filter.Target)
	}

	query := fmt.Sprintf("SELECT %s FROM tasks", allTaskColumns)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY enqueued_at ASC"
	if filter.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, filter.Limit, filter.Offset)
	}

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

// CountTasks returns the number of tasks matching the filter (ignoring
// Limit/Offset) — the pagination envelope's `total` (D20).
func (s *Store) CountTasks(ctx context.Context, filter TaskFilter) (int, error) {
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
	if filter.Target != "" {
		where = append(where, "target = ?")
		args = append(args, filter.Target)
	}
	query := "SELECT COUNT(*) FROM tasks"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListTasksForJobs returns tasks belonging to the given job IDs.
// Returns nil, nil for an empty input slice.
func (s *Store) ListTasksForJobs(ctx context.Context, jobIDs []string) ([]TaskRow, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(jobIDs))
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("SELECT %s FROM tasks WHERE job_id IN (%s) ORDER BY enqueued_at ASC",
		allTaskColumns, strings.Join(placeholders, ","))
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

// GetJobIDsForTasks returns a taskID→jobID map for the given task IDs.
// Unknown task IDs are silently omitted from the result.
func (s *Store) GetJobIDsForTasks(ctx context.Context, taskIDs []string) (map[string]string, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(taskIDs))
	args := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("SELECT id, job_id FROM tasks WHERE id IN (%s)",
		strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string, len(taskIDs))
	for rows.Next() {
		var taskID, jobID string
		if err := rows.Scan(&taskID, &jobID); err != nil {
			return nil, err
		}
		result[taskID] = jobID
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

// ListFinishedTasksAfter returns tasks in terminal states finished after the cutoff.
// Used by fair-share scheduling to compute per-user GPU-hours in a sliding window.
func (s *Store) ListFinishedTasksAfter(ctx context.Context, cutoff time.Time) ([]TaskRow, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM tasks
		 WHERE status IN ('success', 'failed', 'killed')
		   AND finished_at IS NOT NULL
		   AND finished_at >= ?
		 ORDER BY finished_at ASC`, allTaskColumns)

	rows, err := s.db.QueryContext(ctx, query, cutoff.Unix())
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

// ListFinishedTasksBefore returns tasks in terminal states finished before the cutoff.
// Terminal states: success, failed, killed.
func (s *Store) ListFinishedTasksBefore(ctx context.Context, cutoff time.Time) ([]TaskRow, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM tasks
		 WHERE status IN ('success', 'failed', 'killed')
		   AND finished_at IS NOT NULL
		   AND finished_at < ?
		 ORDER BY finished_at ASC`, allTaskColumns)

	rows, err := s.db.QueryContext(ctx, query, cutoff.Unix())
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

// DeleteTask removes a single task by ID.
func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", taskID)
	return err
}

// ListArchivedTasks returns tasks belonging to archived jobs.
func (s *Store) ListArchivedTasks(ctx context.Context) ([]TaskRow, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM tasks WHERE job_id IN (
			SELECT id FROM jobs WHERE archived_at IS NOT NULL
		) ORDER BY enqueued_at ASC`, allTaskColumns)
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

// ListFinishedTasks returns all tasks in terminal states (no time filter).
func (s *Store) ListFinishedTasks(ctx context.Context) ([]TaskRow, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM tasks
		 WHERE status IN ('success', 'failed', 'killed')
		 ORDER BY finished_at ASC`, allTaskColumns)
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

// DeleteOrphanJobs removes terminal-status jobs that have no remaining tasks.
func (s *Store) DeleteOrphanJobs(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM jobs WHERE status IN %s
		 AND id NOT IN (SELECT DISTINCT job_id FROM tasks)`, TerminalJobStatusesSQL()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
