package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/gliese129/runq/internal/store"
)

// ReapResult summarizes what was ingested from <task_dir>/metrics.jsonl
// after a task exit. Counts are informational (logged by reapMetrics);
// no decision logic depends on them.
type ReapResult struct {
	MetricCount     int
	CheckpointCount int
}

// SDK contract for metrics.jsonl events:
//
//	{"type":"metric","key":"loss","value":0.42,"step":100,"ts":1700000000}
//	{"type":"checkpoint","path":"...","size_bytes":1024,"step":100,"is_best":true,"ts":...}
//
// `task_id` / `job_id` are NOT in the SDK payload — daemon fills them
// from the running Task context, because the SDK already has enough
// trouble computing the rest. Keeping the SDK ignorant of internal IDs
// also means we can rename store.MetricRow / store.CheckpointRow without
// breaking the SDK.
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

// ReapTaskOutputs reads <task.TaskDir>/metrics.jsonl, parses each line as a
// typed event, and batch-inserts metric / checkpoint rows into the store.
//
// Recognized event shapes (line-delimited JSON):
//
//	{"type":"metric",     ...metricEvent fields...}
//	{"type":"checkpoint", ...checkpointEvent fields...}
//
// `type` is required; any other value is logged at debug and skipped —
// forward compat for SDK additions. Per-line JSON parse errors are logged
// at warn and the line is skipped (the rest of the file still gets
// processed).
//
// Notes:
//   - Missing file → (ReapResult{}, nil). Empty / no-SDK tasks are normal.
//   - bufio.Scanner buffer is raised to 10 MB to tolerate long metric rows.
//   - INSERT OR IGNORE in the batch methods makes re-reaping idempotent —
//     matters for the MonitorReattached path after daemon restart.
//   - `ts` from the SDK is preserved as-is; daemon does not re-stamp.
//   - Reap MUST NOT propagate errors that would change the task's terminal
//     status. The caller (reapMetrics) treats any error as warn-only.
//
// disk_low events are NOT consumed here — in the SDK-driven freeze model
// the SDK posts /api/internal/freeze-self at runtime; by the time
// metrics.jsonl is reaped the freeze decision is long gone. If an old
// metrics.jsonl from a pre-pivot task still has a disk_low line, it
// falls through to the "unknown type" branch and gets logged at debug.
func ReapTaskOutputs(ctx context.Context, st *store.Store, task *Task) (ReapResult, error) {
	path := filepath.Join(task.TaskDir, "metrics.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return ReapResult{}, nil
	}
	if err != nil {
		return ReapResult{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	metrics := make([]store.MetricRow, 0)
	ckpts := make([]store.CheckpointRow, 0)
	result := ReapResult{}

	// Decode just the type discriminator first, then a typed struct.
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
				TaskID: task.ID,
				JobID:  task.JobID,
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
				TaskID:    task.ID,
				JobID:     task.JobID,
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
