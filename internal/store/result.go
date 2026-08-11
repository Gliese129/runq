package store

import (
	"context"
	"database/sql"
)

// Ingest file identifiers for file_ingest marks (three-file contract, RQ2-1).
const (
	IngestFileResults = "results"
	IngestFileEvents  = "events"
)

// MaxResultRecordsPerTask caps how many result records one task may ingest.
// The cap is a MISUSE guard, not capacity planning: the record contract is
// one row per eval checkpoint (typically a few hundred per task); hitting
// 10k means someone pointed runq.record at a training inner loop. Overflow
// rows are dropped and tallied in file_ingest.dropped_count so the results
// endpoint can report `truncated` honestly.
const MaxResultRecordsPerTask = 10000

// ResultRecordRow maps one line of results.jsonl:
//
//	{"ts":1700000000,"axes":{"model":"a","step":2000},"metrics":{"math":24.8}}
//
// axes/metrics stay opaque JSON here — key classification (identity/x/label)
// is the results endpoint's assembly concern, not storage's.
type ResultRecordRow struct {
	TaskID      string
	JobID       string
	TS          int64
	AxesJSON    string // compact JSON object
	MetricsJSON string // compact JSON object
}

// FileIngestMark is one (task, file) incremental-ingest state — same
// semantics as IngestMark plus the results cap's overflow tally.
type FileIngestMark struct {
	Size    int64
	Offset  int64
	Final   bool
	Dropped int64
}

// GetFileIngestMark returns the (task, file) mark; zero value when absent.
func (s *Store) GetFileIngestMark(ctx context.Context, taskID, file string) (FileIngestMark, error) {
	var m FileIngestMark
	var final int
	err := s.db.QueryRowContext(ctx,
		`SELECT file_size, parsed_offset, final, dropped_count
		 FROM file_ingest WHERE task_id = ? AND file = ?`,
		taskID, file).Scan(&m.Size, &m.Offset, &final, &m.Dropped)
	if err == sql.ErrNoRows {
		return FileIngestMark{}, nil
	}
	if err != nil {
		return FileIngestMark{}, err
	}
	m.Final = final == 1
	return m, nil
}

// SetFileIngestMark upserts the (task, file) mark. Used by the missing-file
// freeze path; the reap's normal delta commits go through the Apply*Delta
// transactions instead.
func (s *Store) SetFileIngestMark(ctx context.Context, taskID, file string, m FileIngestMark) error {
	_, err := s.db.ExecContext(ctx, upsertFileIngestSQL,
		taskID, file, m.Size, m.Offset, boolToInt(m.Final), m.Dropped)
	return err
}

const upsertFileIngestSQL = `
	INSERT INTO file_ingest (task_id, file, file_size, parsed_offset, final, dropped_count)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(task_id, file) DO UPDATE SET
		file_size     = excluded.file_size,
		parsed_offset = excluded.parsed_offset,
		final         = excluded.final,
		dropped_count = excluded.dropped_count`

// ApplyResultsIngestDelta commits one incremental pass over results.jsonl
// ATOMICALLY: optional rebuild delete, cap-limited inserts, and the mark
// land in one transaction (same double-count defense as ApplyIngestDelta —
// result_records has no natural PK, so exactly-once is entirely the mark's
// doing).
//
// parsedTotal is how many valid records the pass parsed; rows carries at
// most MaxResultRecordsPerTask of them (the caller stops materializing
// beyond the cap — the DB can never have room for more). The difference
// between parsedTotal and what actually fits feeds dropped_count.
func (s *Store) ApplyResultsIngestDelta(ctx context.Context, taskID string, rebuild bool,
	rows []ResultRecordRow, parsedTotal int, mark FileIngestMark) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	priorDropped := int64(0)
	if rebuild {
		if _, err := tx.ExecContext(ctx, `DELETE FROM result_records WHERE task_id = ?`, taskID); err != nil {
			return err
		}
	} else {
		// Carry the running tally forward; a rebuild recounts from zero.
		var d sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT dropped_count FROM file_ingest WHERE task_id = ? AND file = ?`,
			taskID, IngestFileResults).Scan(&d)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		priorDropped = d.Int64
	}

	var existing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM result_records WHERE task_id = ?`, taskID).Scan(&existing); err != nil {
		return err
	}
	room := MaxResultRecordsPerTask - existing
	if room < 0 {
		room = 0
	}
	insertN := min(len(rows), room)

	if insertN > 0 {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO result_records (task_id, job_id, ts, axes_json, metrics_json)
			VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows[:insertN] {
			if _, err := stmt.ExecContext(ctx, r.TaskID, r.JobID, r.TS, r.AxesJSON, r.MetricsJSON); err != nil {
				return err
			}
		}
	}

	mark.Dropped = priorDropped + int64(parsedTotal-insertN)
	if _, err := tx.ExecContext(ctx, upsertFileIngestSQL,
		taskID, IngestFileResults, mark.Size, mark.Offset, boolToInt(mark.Final), mark.Dropped); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyEventsIngestDelta commits one incremental pass over events.jsonl:
// checkpoint upserts + the mark in one transaction. No rebuild delete —
// checkpoints are a task-lifetime event log (see DeleteTaskMetrics), and
// the (task_id, step) newer-ts upsert makes full re-reads idempotent.
func (s *Store) ApplyEventsIngestDelta(ctx context.Context, taskID string,
	ckpts []CheckpointRow, mark FileIngestMark) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertCheckpointsTx(ctx, tx, ckpts); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, upsertFileIngestSQL,
		taskID, IngestFileEvents, mark.Size, mark.Offset, boolToInt(mark.Final), mark.Dropped); err != nil {
		return err
	}
	return tx.Commit()
}

// SumResultsDropped totals the results-cap overflow across a job's tasks —
// the honest input for the results endpoint's skipped/truncated fields.
func (s *Store) SumResultsDropped(ctx context.Context, jobID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(fi.dropped_count), 0)
		FROM file_ingest fi
		JOIN tasks t ON t.id = fi.task_id
		WHERE t.job_id = ? AND fi.file = ?`, jobID, IngestFileResults).Scan(&n)
	return n, err
}

// ListResultRecords returns a job's result records ordered by task then
// record order — the raw material for the results endpoint's columnar
// assembly (RQ2-1 c3).
func (s *Store) ListResultRecords(ctx context.Context, jobID string) ([]ResultRecordRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, job_id, ts, axes_json, metrics_json
		 FROM result_records WHERE job_id = ?
		 ORDER BY task_id, rowid`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResultRecordRow
	for rows.Next() {
		var r ResultRecordRow
		if err := rows.Scan(&r.TaskID, &r.JobID, &r.TS, &r.AxesJSON, &r.MetricsJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
