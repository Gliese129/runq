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

// ReapTaskOutputs reads <task.TaskDir>/metrics.jsonl, parses each line as a
// typed event, and batch-inserts metric / checkpoint rows into the store.
//
// Recognized event shapes (line-delimited JSON):
//
//	{"type":"metric",     "data": {...store.MetricRow JSON...}}
//	{"type":"checkpoint", "data": {...store.CheckpointRow JSON...}}
//
// Any other "type" value is logged at debug and skipped — forward compat
// for SDK additions. Per-line JSON parse errors are logged at warn and the
// line is skipped (the rest of the file still gets processed).
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
// Note: disk_low events are NOT consumed here anymore. In the SDK-driven
// freeze model the SDK posts /api/internal/freeze-self at runtime; by the
// time metrics.jsonl is reaped the freeze decision is long gone. If an
// old metrics.jsonl from a pre-pivot task still has a disk_low line, it
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

	lineN := 0
	for scanner.Scan() {
		lineN++
		lineBytes := scanner.Bytes()
		if len(lineBytes) == 0 {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(lineBytes, &raw); err != nil {
			logger.Warnf("reap %s line %d: parse event failed: %v", path, lineN, err)
			continue
		}
		var typ string
		if err := json.Unmarshal(raw["type"], &typ); err != nil {
			logger.Warnf("reap %s line %d: parse type failed: %v", path, lineN, err)
			continue
		}
		switch typ {
		case "metric":
			var data store.MetricRow
			if err := json.Unmarshal(raw["data"], &data); err != nil {
				logger.Warnf("reap %s line %d: parse metric data failed: %v", path, lineN, err)
				continue
			}
			metrics = append(metrics, data)
			result.MetricCount++
		case "checkpoint":
			var data store.CheckpointRow
			if err := json.Unmarshal(raw["data"], &data); err != nil {
				logger.Warnf("reap %s line %d: parse checkpoint data failed: %v", path, lineN, err)
				continue
			}
			ckpts = append(ckpts, data)
			result.CheckpointCount++
		default:
			logger.Debugf("reap %s line %d: unknown event type %q (skipped)", path, lineN, typ)
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
