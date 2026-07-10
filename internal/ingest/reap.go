package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

// Target identifies the task workspace whose SDK outputs should be ingested.
type Target struct {
	TaskID string
	JobID  string
	Dir    string

	// FS is the filesystem Dir lives on. nil = local (os semantics). Remote
	// backends pass their rfs.SSHFS so metrics.jsonl on the cluster is
	// actually readable — a bare os.Open of a remote path silently reads
	// nothing.
	FS rfs.FS
}

// Result summarizes what was ingested from <task_dir>/metrics.jsonl.
// Counts are informational; no decision logic depends on them.
type Result struct {
	MetricCount     int
	CheckpointCount int
}

// SDK contract for metrics.jsonl events:
//
//	{"type":"metric","key":"loss","value":0.42,"step":100,"ts":1700000000}
//	{"type":"checkpoint","path":"...","size_bytes":1024,"step":100,"is_best":true,"ts":...}
//
// `task_id` / `job_id` are NOT in the SDK payload. The caller supplies them
// through Target, keeping the SDK ignorant of internal DB schemas.
type metricEvent struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	Step  *int64  `json:"step"`
	TS    int64   `json:"ts"`
}

type checkpointEvent struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Step      *int64 `json:"step"`
	IsBest    bool   `json:"is_best"`
	TS        int64  `json:"ts"`
}

// NOTE: the legacy ReapOutputs (markless full-file reap) is GONE. Its
// "idempotent via INSERT OR IGNORE" contract silently broke when the
// point table died: streaming summaries ACCUMULATE, so a markless re-read
// double-counts. ReapIncremental's (size, parsed_offset) mark is what
// makes re-reading safe now — every caller must go through it.
//
// SDK event vocabulary and error posture (unchanged):
//   - {"type":"metric"|"checkpoint", ...}; unknown types are debug-logged
//     and skipped (forward compatibility), bad JSON lines warn + skip.
//   - Reap MUST NOT propagate errors that would change a task's terminal
//     status: verdict-path callers treat returned errors as warn-only.
//   - disk_low events fall through to "unknown type": the SDK-driven
//     freeze model handles them at runtime via /internal/freeze-self.

// collector performs the STREAMING REDUCTION over metrics.jsonl lines —
// the ONE parsing path shared by the full reap and the incremental reap.
// Raw metric points are never stored (a chatty 3-day task is 10M+ points):
// each point folds into its (key) summary on the fly; charts read the raw
// tail window or the on-target pyramid index instead.
type collector struct {
	target Target
	path   string
	lineN  int
	aggs   map[string]*store.MetricSummaryRow
	ckpts  []store.CheckpointRow
	result Result
}

func (c *collector) line(lineBytes []byte) {
	c.lineN++
	if len(lineBytes) == 0 {
		return
	}
	var t struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(lineBytes, &t); err != nil {
		logger.Warnf("reap %s line %d: parse type discriminator failed: %v", c.path, c.lineN, err)
		return
	}
	switch t.Type {
	case "metric":
		var e metricEvent
		if err := json.Unmarshal(lineBytes, &e); err != nil {
			logger.Warnf("reap %s line %d: parse metric event failed: %v", c.path, c.lineN, err)
			return
		}
		if c.aggs == nil {
			c.aggs = make(map[string]*store.MetricSummaryRow)
		}
		agg, ok := c.aggs[e.Key]
		if !ok {
			c.aggs[e.Key] = &store.MetricSummaryRow{
				TaskID: c.target.TaskID, JobID: c.target.JobID, Key: e.Key,
				Min: e.Value, Max: e.Value, Last: e.Value, LastTS: e.TS, Count: 1,
			}
		} else {
			agg.Min = min(agg.Min, e.Value)
			agg.Max = max(agg.Max, e.Value)
			if e.TS >= agg.LastTS {
				agg.Last, agg.LastTS = e.Value, e.TS
			}
			agg.Count++
		}
		c.result.MetricCount++
	case "checkpoint":
		var e checkpointEvent
		if err := json.Unmarshal(lineBytes, &e); err != nil {
			logger.Warnf("reap %s line %d: parse checkpoint event failed: %v", c.path, c.lineN, err)
			return
		}
		c.ckpts = append(c.ckpts, store.CheckpointRow{
			TaskID:    c.target.TaskID,
			JobID:     c.target.JobID,
			Path:      e.Path,
			SizeBytes: e.SizeBytes,
			Step:      e.Step,
			IsBest:    e.IsBest,
			TS:        e.TS,
		})
		c.result.CheckpointCount++
	default:
		logger.Debugf("reap %s line %d: unknown event type %q (skipped)", c.path, c.lineN, t.Type)
	}
}

// summaryRows materializes the per-key reductions for the atomic apply.
func (c *collector) summaryRows() []store.MetricSummaryRow {
	rows := make([]store.MetricSummaryRow, 0, len(c.aggs))
	for _, a := range c.aggs {
		rows = append(rows, *a)
	}
	return rows
}

// ReapIncremental ingests only the bytes appended since the last pass,
// tracked by the (size, parsed_offset) mark (spec §8.1.4):
//
//	size unchanged → zero transfer, zero parse (the common idle case)
//	size grew      → seek to parsed_offset, parse the delta's COMPLETE
//	                 lines (an unterminated tail waits for the next pass)
//	size shrank    → the file was rewritten (retry rerun): drop the task's
//	                 rows and rebuild from 0
//
// final marks the terminal pass: the tail is consumed even without a
// trailing newline, and the mark is frozen — settled tasks are never
// stat'ed again. Errors are warn-only for callers on verdict paths (same
// contract as the old full reap).
func ReapIncremental(ctx context.Context, st *store.Store, target Target, final bool) (Result, error) {
	fsys := target.FS
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	mark, err := st.GetIngestMark(ctx, target.TaskID)
	if err != nil {
		return Result{}, err
	}
	if mark.Final {
		return Result{}, nil
	}

	path := workspace.MetricsPath(target.Dir)
	info, err := fsys.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if final { // no-SDK task: freeze so we never look again
			_ = st.SetIngestMark(ctx, target.TaskID, store.IngestMark{Final: true})
		}
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}

	size := info.Size()
	offset := mark.Offset
	rebuild := false
	if size < mark.Size {
		// Rewritten file (retry rerun): re-read from 0. The DELETE of the
		// old summaries is DEFERRED into the atomic apply below — reading
		// happens against a still-consistent store, and a crash at any
		// point leaves either the old state or the new, never a mix.
		rebuild = true
		offset = 0
	} else if size == mark.Size && !final {
		return Result{}, nil // zero transfer
	}

	f, err := fsys.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return Result{}, err
		}
	}

	br := bufio.NewReaderSize(f, 64*1024)
	c := collector{target: target, path: path}
	consumed := offset
	for {
		raw, rerr := br.ReadBytes('\n')
		complete := rerr == nil
		if len(raw) > 0 && (complete || final) {
			c.line(bytes.TrimRight(raw, "\r\n"))
			consumed += int64(len(raw))
		}
		if rerr != nil {
			if rerr != io.EOF {
				logger.Warnf("reap %s: read error (partial result): %v", path, rerr)
			}
			break
		}
	}
	// ONE transaction: rebuild delete + summary merges + checkpoints + the
	// mark. "Counted" and "accounted" are inseparable — a crash between
	// them would replay this delta next pass and inflate count/sum.
	if err := st.ApplyIngestDelta(ctx, target.TaskID, rebuild,
		c.summaryRows(), c.ckpts,
		store.IngestMark{Size: size, Offset: consumed, Final: final}); err != nil {
		return Result{}, err
	}
	return c.result, nil
}
