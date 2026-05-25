package store

import (
	"context"
	"database/sql"
)

// MetricRow maps to one row in the `metrics` table.
// Populated by the daemon when reading <task_dir>/metrics.jsonl after a task
// exits (see scheduler reap logic). Step is nullable because user scripts that
// don't track step (eval-only scripts, single-shot inference) still log metrics.
type MetricRow struct {
	TaskID string
	JobID  string
	Key    string
	Value  float64
	Step   *int64 // nil → SQL NULL
	TS     int64  // Unix timestamp; daemon preserves the value SDK wrote, doesn't re-stamp
}

// CheckpointRow maps to one row in the `checkpoints` table.
// Populated when daemon sees a `type=checkpoint` event in metrics.jsonl
// (written by @runq.safe_save on the SDK side).
type CheckpointRow struct {
	TaskID    string
	JobID     string
	Path      string
	SizeBytes int64
	Step      *int64
	IsBest    bool
	TS        int64
}

// InsertMetricsBatch inserts many MetricRow within one transaction.
// Uses INSERT OR IGNORE so that re-reaping the same metrics.jsonl (e.g. after
// daemon restart + reclaim) doesn't fail on PK conflicts.
//
// Caller-side guarantees:
//   - All rows must reference an existing task (FK).
//   - Empty slice is a no-op (returns nil).
func (s *Store) InsertMetricsBatch(ctx context.Context, rows []MetricRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO metrics
			(task_id, job_id, key, value, step, ts)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.TaskID, r.JobID, r.Key, r.Value, nullStep(r.Step), r.TS,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertCheckpointsBatch inserts many CheckpointRow within one transaction.
// INSERT OR IGNORE same as metrics — re-reaping must be idempotent.
func (s *Store) InsertCheckpointsBatch(ctx context.Context, rows []CheckpointRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO checkpoints
			(task_id, job_id, path, size_bytes, step, is_best, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		isBest := 0
		if r.IsBest {
			isBest = 1
		}
		if _, err := stmt.ExecContext(ctx,
			r.TaskID, r.JobID, r.Path, r.SizeBytes, nullStep(r.Step), isBest, r.TS,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListMetrics returns metrics for a task, optionally filtered by key.
// Used by stage 3's `runq log <task>` and tests.
func (s *Store) ListMetrics(ctx context.Context, taskID, key string) ([]MetricRow, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if key == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT task_id, job_id, key, value, step, ts
			 FROM metrics WHERE task_id = ? ORDER BY ts ASC`, taskID)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT task_id, job_id, key, value, step, ts
			 FROM metrics WHERE task_id = ? AND key = ? ORDER BY ts ASC`, taskID, key)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MetricRow
	for rows.Next() {
		var (
			m    MetricRow
			step sql.NullInt64
		)
		if err := rows.Scan(&m.TaskID, &m.JobID, &m.Key, &m.Value, &step, &m.TS); err != nil {
			return nil, err
		}
		if step.Valid {
			v := step.Int64
			m.Step = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MaxCheckpointSize returns the largest size_bytes recorded for the task in
// the checkpoints table, or 0 if the task has no checkpoint history (e.g. it
// is not using the runq Python SDK's @runq.safe_save, or hasn't saved yet).
//
// Used by the scheduler to decide whether to include the task in a selective
// freeze: tasks with no history are intentionally left running (no signal
// they would fail), tasks whose max × safety_factor exceeds free bytes are
// frozen.
func (s *Store) MaxCheckpointSize(ctx context.Context, taskID string) (int64, error) {
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(size_bytes) FROM checkpoints WHERE task_id = ?`, taskID,
	).Scan(&size)
	if err != nil {
		return 0, err
	}
	if !size.Valid {
		return 0, nil
	}
	return size.Int64, nil
}

// LatestCheckpoint returns the most recent checkpoint for the task (highest
// step, or highest ts if step is null), or nil if none exists. Used by the
// thaw handler to show "you just wrote X bytes" alongside blocked tasks.
func (s *Store) LatestCheckpoint(ctx context.Context, taskID string) (*CheckpointRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT task_id, job_id, path, size_bytes, step, is_best, ts
		 FROM checkpoints
		 WHERE task_id = ?
		 ORDER BY COALESCE(step, -1) DESC, ts DESC
		 LIMIT 1`, taskID)
	var (
		c       CheckpointRow
		step    sql.NullInt64
		isBest  int
		sizeRaw sql.NullInt64
	)
	if err := row.Scan(&c.TaskID, &c.JobID, &c.Path, &sizeRaw, &step, &isBest, &c.TS); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	c.SizeBytes = sizeRaw.Int64
	if step.Valid {
		v := step.Int64
		c.Step = &v
	}
	c.IsBest = isBest != 0
	return &c, nil
}

// TotalCheckpointSize returns SUM(size_bytes) over all checkpoints recorded
// for the task — the cumulative bytes this task has written to its ckpt
// dir. Used by the thaw handler to rank "disk hogs" when listing mount mates.
// Returns 0 if the task has no checkpoint history.
func (s *Store) TotalCheckpointSize(ctx context.Context, taskID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(size_bytes) FROM checkpoints WHERE task_id = ?`, taskID,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// ListCheckpoints returns all checkpoints for a task, ordered by step.
func (s *Store) ListCheckpoints(ctx context.Context, taskID string) ([]CheckpointRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, job_id, path, size_bytes, step, is_best, ts
		 FROM checkpoints WHERE task_id = ? ORDER BY step ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CheckpointRow
	for rows.Next() {
		var (
			c       CheckpointRow
			step    sql.NullInt64
			isBest  int
			sizeRaw sql.NullInt64
		)
		if err := rows.Scan(&c.TaskID, &c.JobID, &c.Path, &sizeRaw, &step, &isBest, &c.TS); err != nil {
			return nil, err
		}
		c.SizeBytes = sizeRaw.Int64
		if step.Valid {
			v := step.Int64
			c.Step = &v
		}
		c.IsBest = isBest != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// nullStep converts *int64 to sql.NullInt64 for nullable step columns.
func nullStep(s *int64) sql.NullInt64 {
	if s == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *s, Valid: true}
}
