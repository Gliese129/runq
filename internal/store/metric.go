package store

import (
	"context"
	"database/sql"
	"strings"
)

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

// IngestMark is one task's incremental-ingest state (spec §8.1.4).
// Zero value = never ingested.
type IngestMark struct {
	Size   int64 // stat size at last pass
	Offset int64 // parsed up to here (complete lines only)
	Final  bool  // terminal pass done: skip this task forever
}

// GetIngestMark returns the task's mark; zero value when absent.
func (s *Store) GetIngestMark(ctx context.Context, taskID string) (IngestMark, error) {
	var m IngestMark
	var final int
	err := s.db.QueryRowContext(ctx,
		`SELECT file_size, parsed_offset, final FROM metrics_ingest WHERE task_id = ?`,
		taskID).Scan(&m.Size, &m.Offset, &final)
	if err == sql.ErrNoRows {
		return IngestMark{}, nil
	}
	if err != nil {
		return IngestMark{}, err
	}
	m.Final = final == 1
	return m, nil
}

// SetIngestMark upserts the task's mark.
func (s *Store) SetIngestMark(ctx context.Context, taskID string, m IngestMark) error {
	final := 0
	if m.Final {
		final = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO metrics_ingest (task_id, file_size, parsed_offset, final)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			file_size = excluded.file_size,
			parsed_offset = excluded.parsed_offset,
			final = excluded.final`,
		taskID, m.Size, m.Offset, final)
	return err
}

// DeleteTaskMetrics drops a task's ingested SDK-output state — summaries,
// result records, and every ingest mark — the retry unfreeze ("fresh
// attempt = fresh ingest"). Terminal passes froze the marks (Final=true);
// without this reset the new attempt's files would never be read. Result
// rows must go WITH their mark: result_records has no natural PK, so a
// mark-only reset would double-insert them on the full re-read.
//
// checkpoints are DELIBERATELY kept: they are a TASK-lifetime event log,
// not a reduction of the current file. Freeze sizing (MaxCheckpointSize)
// asks "how big does this task's checkpoints get" — attempt-independent,
// and a retry often RESUMES from the previous attempt's checkpoint, whose
// file is still on disk. PK (task_id, step) + newer-ts upsert makes
// re-reads idempotent, so keeping rows costs nothing.
func (s *Store) DeleteTaskMetrics(ctx context.Context, taskID string) error {
	for _, stmt := range []string{
		`DELETE FROM metric_summary WHERE task_id = ?`,
		`DELETE FROM metrics_ingest WHERE task_id = ?`,
		`DELETE FROM result_records WHERE task_id = ?`,
		`DELETE FROM file_ingest WHERE task_id = ?`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt, taskID); err != nil {
			return err
		}
	}
	return nil
}

// MetricSummaryRow is one (task, key) streaming reduction.
type MetricSummaryRow struct {
	TaskID string
	JobID  string
	Key    string
	Min    float64
	Max    float64
	Last   float64
	LastTS int64
	Count  int64
}

// MergeMetricSummaries upserts delta reductions into metric_summary.
// min/max/count merge associatively; last wins by timestamp — so repeated
// incremental passes compose losslessly.
func (s *Store) MergeMetricSummaries(ctx context.Context, rows []MetricSummaryRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := mergeMetricSummariesTx(ctx, tx, rows); err != nil {
		return err
	}
	return tx.Commit()
}

func mergeMetricSummariesTx(ctx context.Context, tx *sql.Tx, rows []MetricSummaryRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO metric_summary (task_id, job_id, key, min, max, last, last_ts, count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id, key) DO UPDATE SET
			min     = MIN(metric_summary.min, excluded.min),
			max     = MAX(metric_summary.max, excluded.max),
			last    = CASE WHEN excluded.last_ts >= metric_summary.last_ts
			               THEN excluded.last ELSE metric_summary.last END,
			last_ts = MAX(metric_summary.last_ts, excluded.last_ts),
			count   = metric_summary.count + excluded.count`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.TaskID, r.JobID, r.Key, r.Min, r.Max, r.Last, r.LastTS, r.Count); err != nil {
			return err
		}
	}
	return nil
}

// ApplyIngestDelta commits one incremental-reap pass ATOMICALLY: optional
// rebuild delete, summary merges, checkpoint inserts, and the ingest mark
// land in ONE transaction. This is the double-count defense — "counted"
// and "accounted" must be inseparable: a crash between merging a delta
// and advancing the mark would replay the delta and inflate count/sum
// (the exact disease that killed the markless full reap).
func (s *Store) ApplyIngestDelta(ctx context.Context, taskID string, rebuild bool,
	summaries []MetricSummaryRow, ckpts []CheckpointRow, mark IngestMark) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if rebuild {
		// Summaries only — checkpoints are a task-lifetime event log (see
		// DeleteTaskMetrics), and re-reading the rewritten file re-inserts
		// its events idempotently via PK (task_id, step).
		if _, err := tx.ExecContext(ctx, `DELETE FROM metric_summary WHERE task_id = ?`, taskID); err != nil {
			return err
		}
	}
	if err := mergeMetricSummariesTx(ctx, tx, summaries); err != nil {
		return err
	}
	if err := insertCheckpointsTx(ctx, tx, ckpts); err != nil {
		return err
	}
	final := 0
	if mark.Final {
		final = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO metrics_ingest (task_id, file_size, parsed_offset, final)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			file_size = excluded.file_size,
			parsed_offset = excluded.parsed_offset,
			final = excluded.final`,
		taskID, mark.Size, mark.Offset, final); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMetricSummaries returns a job's summaries, optionally one key only.
func (s *Store) ListMetricSummaries(ctx context.Context, jobID, key string) ([]MetricSummaryRow, error) {
	query := `SELECT task_id, job_id, key, min, max, last, last_ts, count FROM metric_summary WHERE job_id = ?`
	args := []any{jobID}
	if key != "" {
		query += ` AND key = ?`
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricSummaryRow
	for rows.Next() {
		var r MetricSummaryRow
		if err := rows.Scan(&r.TaskID, &r.JobID, &r.Key, &r.Min, &r.Max, &r.Last, &r.LastTS, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskCleanStat is the ledger-known size preview for clean's TUI: what
// deletion would free, WITHOUT touching the filesystem.
type TaskCleanStat struct {
	CkptFiles    int
	CkptBytes    int64
	MetricsBytes int64
}

// TaskCleanStats batch-fetches clean previews for the given task IDs
// (chunked IN queries; checkpoints + ingest marks).
func (s *Store) TaskCleanStats(ctx context.Context, taskIDs []string) (map[string]TaskCleanStat, error) {
	out := make(map[string]TaskCleanStat, len(taskIDs))
	const chunk = 500
	for start := 0; start < len(taskIDs); start += chunk {
		ids := taskIDs[start:min(start+chunk, len(taskIDs))]
		ph := strings.Repeat("?,", len(ids)-1) + "?"
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}

		rows, err := s.db.QueryContext(ctx,
			`SELECT task_id, COUNT(*), COALESCE(SUM(size_bytes), 0)
			 FROM checkpoints WHERE task_id IN (`+ph+`) GROUP BY task_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var st TaskCleanStat
			if err := rows.Scan(&id, &st.CkptFiles, &st.CkptBytes); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = st
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		mrows, err := s.db.QueryContext(ctx,
			`SELECT task_id, file_size FROM metrics_ingest WHERE task_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var id string
			var size int64
			if err := mrows.Scan(&id, &size); err != nil {
				mrows.Close()
				return nil, err
			}
			st := out[id]
			st.MetricsBytes = size
			out[id] = st
		}
		mrows.Close()
		if err := mrows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MetricKeys is the key-discovery query (spec §5.4 dual-mode): DISTINCT
// over summaries — O(tasks × keys) rows, tiny by construction.
func (s *Store) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT key FROM metric_summary WHERE job_id = ? ORDER BY key`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
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
	if err := insertCheckpointsTx(ctx, tx, rows); err != nil {
		return err
	}
	return tx.Commit()
}

// insertCheckpointsTx upserts checkpoint events: PK (task_id, step),
// NEWER ts wins. A retry rerun re-saving the same step must update
// size/path/is_best (the old OR IGNORE kept the stale row — conservative
// for MaxCheckpointSize but wrong for LatestCheckpoint's thaw display);
// the ts guard keeps out-of-order re-reads from clobbering newer facts,
// which also preserves re-read idempotence.
func insertCheckpointsTx(ctx context.Context, tx *sql.Tx, rows []CheckpointRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO checkpoints
			(task_id, job_id, path, size_bytes, step, is_best, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id, step) DO UPDATE SET
			path       = excluded.path,
			size_bytes = excluded.size_bytes,
			is_best    = excluded.is_best,
			ts         = excluded.ts
		WHERE excluded.ts >= checkpoints.ts`)
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
	return nil
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
