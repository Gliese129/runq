package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskRow maps to the `tasks` table in SQLite.
// Unlike scheduler.Task (an in-memory scheduling object with runtime fields like Env map),
// TaskRow is a pure data-transfer object whose field types mirror the DB schema.
// Callers are responsible for converting between the two.
type TaskRow struct {
	ID            string
	JobID         string
	ProjectName   string
	Command       string
	ParamsJSON    string // JSON-serialized parameter map
	GPUsNeeded    int
	GPUs          string // comma-separated GPU indices, e.g. "0,1,3"
	Status        string
	KillRequested bool
	RetryCount    int
	MaxRetry      int
	PID           int
	StartTime     int64 // /proc starttime (Unix timestamp) for reclaim validation
	LogPath       string
	WorkingDir    string
	EnvJSON       string // JSON-serialized environment variable map
	Resumable     bool
	ExtraArgs     string
	UID           int // submitting user's UID
	Timeout       int // timeout in seconds, 0 = no timeout
	EnqueuedAt    time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time

	// L2-C: per-task workspace at <root>/<job_id>/<task_id>.
	// Holds params.json, wandb_config.json (optional), metrics.jsonl, checkpoints/.
	// Created by service.JobService.SubmitJob, read by SDK via RUNQ_TASK_DIR env.
	TaskDir string

	// Target is the compute target this task was submitted to (e.g. "local",
	// "tsubame"). Phase 1: MultiBackend routing key.
	Target string

	// L2-E: external attempt/job id returned by runqd or an HPC scheduler.
	// Set after handoff and used by refresh to map a task back to its execution
	// record for status and cancellation.
	ExternalID string

	// L2-E: provenance of Status — "" | wrapper | scheduler | inferred | runq |
	// submit | retry. Reconciliation applies the closed evidence-precedence matrix:
	// wrapper/runq terminals are hard; scheduler/submit/inferred evidence is
	// correctable only by an allowed stronger fact.
	StatusSource string

	// RQ-74: verbatim failure evidence for pre-run failures (submit rejection:
	// scheduler stderr + exit code + rendered command).
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
	// Scope confines the result to rows the querying LANE owns (RQ-75
	// generation isolation). nil = no ownership filter (user-facing list
	// views see everything).
	Scope *LaneScope
}

// allTaskColumns lists every column in the tasks table.
// Defined once so SELECT and Scan stay in sync; adding a column means editing one place.
const allTaskColumns = `id, job_id, project_name, command, params_json,
	gpus_needed, gpus, status, retry_count, max_retry,
	pid, start_time, log_path, working_dir, env_json,
	resumable, extra_args, uid, timeout,
	enqueued_at, started_at, finished_at, task_dir, target, external_id, status_source, kill_requested,
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
		killRequested int
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
		&taskDir, &target, &externalID, &statusSource, &killRequested,
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
	t.KillRequested = killRequested != 0
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
		enqueued_at, started_at, finished_at, task_dir, target, external_id, status_source, kill_requested,
		native_state, queue, failure_detail, target_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	resumable := 0
	if t.Resumable {
		resumable = 1
	}
	killRequested := 0
	if t.KillRequested {
		killRequested = 1
	}

	_, err := s.db.ExecContext(ctx, query,
		t.ID, t.JobID, t.ProjectName, t.Command, t.ParamsJSON,
		t.GPUsNeeded, nullString(t.GPUs), t.Status, t.RetryCount, t.MaxRetry,
		nullInt(t.PID), nullInt64(t.StartTime), nullString(t.LogPath),
		nullString(t.WorkingDir), nullString(t.EnvJSON),
		resumable, t.ExtraArgs, nullInt(t.UID), nullInt(t.Timeout),
		t.EnqueuedAt.Unix(),
		nullTimeToUnix(t.StartedAt), nullTimeToUnix(t.FinishedAt),
		nullString(t.TaskDir), targetOrDefault(t.Target), nullString(t.ExternalID), nullString(t.StatusSource), killRequested,
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
		enqueued_at, started_at, finished_at, task_dir, target, external_id, status_source, kill_requested,
		native_state, queue, failure_detail, target_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	resumable := 0
	if t.Resumable {
		resumable = 1
	}
	killRequested := 0
	if t.KillRequested {
		killRequested = 1
	}

	_, err := tx.ExecContext(ctx, query,
		t.ID, t.JobID, t.ProjectName, t.Command, t.ParamsJSON,
		t.GPUsNeeded, nullString(t.GPUs), t.Status, t.RetryCount, t.MaxRetry,
		nullInt(t.PID), nullInt64(t.StartTime), nullString(t.LogPath),
		nullString(t.WorkingDir), nullString(t.EnvJSON),
		resumable, t.ExtraArgs, nullInt(t.UID), nullInt(t.Timeout),
		t.EnqueuedAt.Unix(),
		nullTimeToUnix(t.StartedAt), nullTimeToUnix(t.FinishedAt),
		nullString(t.TaskDir), targetOrDefault(t.Target), nullString(t.ExternalID), nullString(t.StatusSource), killRequested,
		nullString(t.NativeState), nullString(t.Queue), nullString(t.FailureDetail),
		t.TargetGeneration,
	)
	return err
}

// allowedStatusFields is the whitelist of column names that UpdateTaskStatus
// accepts. Any key not in this set is rejected to prevent accidental SQL
// injection from future callers.
var allowedStatusFields = map[string]bool{
	"pid":               true,
	"gpus":              true,
	"start_time":        true,
	"started_at":        true,
	"finished_at":       true,
	"retry_count":       true,
	"log_path":          true,
	"working_dir":       true,
	"env_json":          true,
	"external_id":       true,
	"status_source":     true,
	"extra_args":        true,
	"native_state":      true,
	"target_generation": true,
	"queue":             true,
	"failure_detail":    true,
	"kill_requested":    true,
}

// ErrTaskStatusConflict means a conditional task transition lost a race with
// another durable transition. Callers should reload the row rather than
// retrying an observation made against the old attempt.
var ErrTaskStatusConflict = errors.New("task status transition conflict")

const (
	expectedStatusField           = "__expected_status"
	expectedStatusSourceField     = "__expected_status_source"
	expectedRetryCountField       = "__expected_retry_count"
	expectedTargetGenerationField = "__expected_target_generation"
	expectedExternalIDField       = "__expected_external_id"
)

// FenceTaskStatusUpdate attaches the durable identity of the task attempt
// that produced a transition. The metadata travels through lifecycle funnels
// in fields but is consumed as WHERE predicates by UpdateTaskStatus; it is
// never written as task data.
func FenceTaskStatusUpdate(fields map[string]any, expected TaskRow) {
	fields[expectedStatusField] = expected.Status
	fields[expectedStatusSourceField] = expected.StatusSource
	fields[expectedRetryCountField] = expected.RetryCount
	fields[expectedTargetGenerationField] = expected.TargetGeneration
	fields[expectedExternalIDField] = expected.ExternalID
}

// CarryTaskStatusFence copies only FenceTaskStatusUpdate's private predicate
// metadata into another transition field map. Lifecycle reducers such as the
// scheduler's failed→pending retry intentionally discard terminal evidence
// fields, but they must not discard the attempt identity that guards the
// write against a stronger concurrent verdict or a newer generation.
func CarryTaskStatusFence(dst, src map[string]any) {
	for _, key := range []string{
		expectedStatusField,
		expectedStatusSourceField,
		expectedRetryCountField,
		expectedTargetGenerationField,
		expectedExternalIDField,
	} {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

// SetTaskKillRequested durably records or clears user cancellation intent on
// a non-terminal task. The conditional update prevents a racing request from
// attaching stale intent after the task has already settled.
func (s *Store) SetTaskKillRequested(ctx context.Context, taskID string, requested bool) (bool, error) {
	value := 0
	if requested {
		value = 1
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET kill_requested = ?
		WHERE id = ? AND status NOT IN ('success', 'failed', 'killed')`, value, taskID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return false, fmt.Errorf("task %q not found", taskID)
		}
		return false, err
	}
	return false, nil
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
	whereClauses := []string{"id = ?"}
	whereArgs := []any{taskID}
	fenced := false

	if expected, ok := fields[expectedStatusField]; ok {
		fenced = true
		whereClauses = append(whereClauses, "status = ?")
		whereArgs = append(whereArgs, expected)
	}
	if expected, ok := fields[expectedStatusSourceField]; ok {
		whereClauses = append(whereClauses, "COALESCE(status_source, '') = ?")
		whereArgs = append(whereArgs, expected)
	}
	if expected, ok := fields[expectedRetryCountField]; ok {
		whereClauses = append(whereClauses, "retry_count = ?")
		whereArgs = append(whereArgs, expected)
	}
	if expected, ok := fields[expectedTargetGenerationField]; ok {
		whereClauses = append(whereClauses, "COALESCE(target_generation, '') = ?")
		whereArgs = append(whereArgs, expected)
	}
	if expected, ok := fields[expectedExternalIDField]; ok {
		whereClauses = append(whereClauses, "COALESCE(external_id, '') = ?")
		whereArgs = append(whereArgs, expected)
	}

	for col, val := range fields {
		switch col {
		case expectedStatusField, expectedStatusSourceField, expectedRetryCountField,
			expectedTargetGenerationField, expectedExternalIDField:
			continue
		}
		if !allowedStatusFields[col] {
			return fmt.Errorf("UpdateTaskStatus: column %q not in whitelist", col)
		}
		setClauses = append(setClauses, col+" = ?")
		args = append(args, val)
	}
	// Automatic failure retry increments retry_count but intentionally drops
	// terminal evidence fields. Requiring the immediately preceding epoch is
	// its attempt fence: a manual retry records the same next epoch first, so
	// an old failure can no longer republish pending over that reset intent.
	if !fenced {
		if next, ok := intField(fields["retry_count"]); ok && next > 0 {
			whereClauses = append(whereClauses, "retry_count = ?")
			whereArgs = append(whereArgs, next-1)
		}
	}

	query := fmt.Sprintf("UPDATE tasks SET %s WHERE %s",
		strings.Join(setClauses, ", "), strings.Join(whereClauses, " AND "))
	args = append(args, whereArgs...)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		if fenced || len(whereClauses) > 1 {
			return fmt.Errorf("%w for task %q", ErrTaskStatusConflict, taskID)
		}
		return fmt.Errorf("task %q not found", taskID)
	}
	return nil
}

func intField(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
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
	if filter.Scope != nil {
		cl, sargs := filter.Scope.whereClause()
		where = append(where, cl)
		args = append(args, sargs...)
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

// ListActiveTasks returns every nonterminal task, ordered by enqueue time.
// Called at daemon startup to rebuild the in-memory Queue.
func (s *Store) ListActiveTasks(ctx context.Context) ([]TaskRow, error) {
	query := fmt.Sprintf(
		"SELECT %s FROM tasks WHERE status IN %s ORDER BY enqueued_at ASC",
		allTaskColumns, ActiveStatusesSQL(),
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
