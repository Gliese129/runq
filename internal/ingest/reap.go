package ingest

import (
	"bufio"
	"context"
	"encoding/json"
	"os"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

// Target identifies the task workspace whose SDK outputs should be ingested.
type Target struct {
	TaskID string
	JobID  string
	Dir    string
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

// ReapOutputs reads <target.Dir>/metrics.jsonl, parses each line as a typed
// event, and batch-inserts metric / checkpoint rows into the store.
//
// Recognized event shapes (line-delimited JSON):
//
//	{"type":"metric",     ...metricEvent fields...}
//	{"type":"checkpoint", ...checkpointEvent fields...}
//
// `type` is required; any other value is logged at debug and skipped for
// forward compatibility. Per-line JSON parse errors are logged at warn and the
// line is skipped; the rest of the file still gets processed.
//
// Notes:
//   - Missing file -> (Result{}, nil). Empty / no-SDK tasks are normal.
//   - bufio.Scanner buffer is raised to 10 MB to tolerate long metric rows.
//   - INSERT OR IGNORE in the store batch methods makes re-reaping idempotent.
//     This is a hard dependency for future daemonless status refresh, which may
//     reread the same jsonl multiple times.
//   - `ts` from the SDK is preserved as-is; ingest does not re-stamp.
//   - Reap MUST NOT propagate errors that would change the task's terminal
//     status. Callers should treat returned errors as warn-only.
//
// disk_low events are NOT consumed here. In the SDK-driven freeze model the SDK
// posts /api/internal/freeze-self at runtime; by the time metrics.jsonl is
// reaped the freeze decision is long gone. If an old jsonl from a pre-pivot
// task still has a disk_low line, it falls through to the "unknown type" branch.
func ReapOutputs(ctx context.Context, st *store.Store, target Target) (Result, error) {
	path := workspace.MetricsPath(target.Dir)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	metrics := make([]store.MetricRow, 0)
	ckpts := make([]store.CheckpointRow, 0)
	result := Result{}

	type typeOnly struct {
		Type string `json:"type"`
	}

	lineN := 0
	for scanner.Scan() {
		lineN++
		lineBytes := scanner.Bytes()
		if len(lineBytes) == 0 {
			continue
		}
		var t typeOnly
		if err := json.Unmarshal(lineBytes, &t); err != nil {
			logger.Warnf("reap %s line %d: parse type discriminator failed: %v", path, lineN, err)
			continue
		}
		switch t.Type {
		case "metric":
			var e metricEvent
			if err := json.Unmarshal(lineBytes, &e); err != nil {
				logger.Warnf("reap %s line %d: parse metric event failed: %v", path, lineN, err)
				continue
			}
			metrics = append(metrics, store.MetricRow{
				TaskID: target.TaskID,
				JobID:  target.JobID,
				Key:    e.Key,
				Value:  e.Value,
				Step:   e.Step,
				TS:     e.TS,
			})
			result.MetricCount++
		case "checkpoint":
			var e checkpointEvent
			if err := json.Unmarshal(lineBytes, &e); err != nil {
				logger.Warnf("reap %s line %d: parse checkpoint event failed: %v", path, lineN, err)
				continue
			}
			ckpts = append(ckpts, store.CheckpointRow{
				TaskID:    target.TaskID,
				JobID:     target.JobID,
				Path:      e.Path,
				SizeBytes: e.SizeBytes,
				Step:      e.Step,
				IsBest:    e.IsBest,
				TS:        e.TS,
			})
			result.CheckpointCount++
		default:
			logger.Debugf("reap %s line %d: unknown event type %q (skipped)", path, lineN, t.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warnf("reap %s: scanner error (partial result): %v", path, err)
	}
	if err := st.InsertMetricsBatch(ctx, metrics); err != nil {
		logger.Warnf("reap %s: InsertMetricsBatch failed (%d rows): %v", path, len(metrics), err)
	}
	if err := st.InsertCheckpointsBatch(ctx, ckpts); err != nil {
		logger.Warnf("reap %s: InsertCheckpointsBatch failed (%d rows): %v", path, len(ckpts), err)
	}
	return result, nil
}
