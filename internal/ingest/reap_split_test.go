package ingest

// Tests for the three-file ingest (RQ2-1): results.jsonl → result_records
// (capped, dropped-tallied), events.jsonl → checkpoints, and the marks that
// make each file's re-read exactly-once. metrics.jsonl behavior (including
// the legacy mixed-file tolerance) is covered by reap_test.go.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/workspace"
)

func writeTaskFile(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestReapResultsNormal(t *testing.T) {
	taskDir := t.TempDir()
	writeTaskFile(t, workspace.ResultsPath(taskDir), []string{
		`{"ts":1700000000,"axes":{"model":"a","step":2000},"metrics":{"math":24.8}}`,
		`{"ts":1700000060,"axes":{"model":"a","step":4000},"metrics":{"math":26.1}}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.ResultCount != 2 {
		t.Errorf("ResultCount = %d, want 2", res.ResultCount)
	}
	rows, err := st.ListResultRecords(context.Background(), "j1")
	if err != nil {
		t.Fatalf("ListResultRecords: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.TaskID != "t1" || r.JobID != "j1" || r.TS != 1700000000 {
		t.Errorf("identity/ts not filled from context: %+v", r)
	}
	if r.AxesJSON != `{"model":"a","step":2000}` {
		t.Errorf("axes_json = %q", r.AxesJSON)
	}
	if r.MetricsJSON != `{"math":24.8}` {
		t.Errorf("metrics_json = %q", r.MetricsJSON)
	}
}

func TestReapResultsIncrementalAndIdempotent(t *testing.T) {
	taskDir := t.TempDir()
	path := workspace.ResultsPath(taskDir)
	writeTaskFile(t, path, []string{
		`{"ts":1,"axes":{"step":1},"metrics":{"m":1}}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)
	target := Target{TaskID: "t1", JobID: "j1", Dir: taskDir}

	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("first reap: %v", err)
	}
	// Unchanged file → zero-transfer pass, no duplicate rows.
	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	rows, _ := st.ListResultRecords(context.Background(), "j1")
	if len(rows) != 1 {
		t.Fatalf("after idempotent pass: row count = %d, want 1", len(rows))
	}

	// Append one record → only the delta lands.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(`{"ts":2,"axes":{"step":2},"metrics":{"m":2}}` + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()
	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("delta reap: %v", err)
	}
	rows, _ = st.ListResultRecords(context.Background(), "j1")
	if len(rows) != 2 {
		t.Fatalf("after delta pass: row count = %d, want 2: %+v", len(rows), rows)
	}
}

func TestReapResultsRebuildOnShrink(t *testing.T) {
	taskDir := t.TempDir()
	path := workspace.ResultsPath(taskDir)
	writeTaskFile(t, path, []string{
		`{"ts":1,"axes":{"step":1},"metrics":{"m":1}}`,
		`{"ts":2,"axes":{"step":2},"metrics":{"m":2}}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)
	target := Target{TaskID: "t1", JobID: "j1", Dir: taskDir}
	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("first reap: %v", err)
	}

	// Retry rerun rewrote the file smaller → rows rebuild, no duplicates.
	writeTaskFile(t, path, []string{
		`{"ts":9,"axes":{"step":1},"metrics":{"m":9}}`,
	})
	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("rebuild reap: %v", err)
	}
	rows, _ := st.ListResultRecords(context.Background(), "j1")
	if len(rows) != 1 {
		t.Fatalf("after rebuild: row count = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].MetricsJSON != `{"m":9}` {
		t.Errorf("rebuild kept stale row: %+v", rows[0])
	}
}

func TestReapResultsCapAndDroppedCount(t *testing.T) {
	taskDir := t.TempDir()
	over := 25
	lines := make([]string, 0, store.MaxResultRecordsPerTask+over)
	for i := 0; i < store.MaxResultRecordsPerTask+over; i++ {
		lines = append(lines, fmt.Sprintf(`{"ts":%d,"axes":{"step":%d},"metrics":{"m":%d}}`, i, i, i))
	}
	writeTaskFile(t, workspace.ResultsPath(taskDir), lines)
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.ResultCount != store.MaxResultRecordsPerTask+over {
		t.Errorf("ResultCount = %d, want %d (parsed pre-cap)", res.ResultCount, store.MaxResultRecordsPerTask+over)
	}
	rows, _ := st.ListResultRecords(context.Background(), "j1")
	if len(rows) != store.MaxResultRecordsPerTask {
		t.Fatalf("row count = %d, want cap %d", len(rows), store.MaxResultRecordsPerTask)
	}
	mark, err := st.GetFileIngestMark(context.Background(), "t1", store.IngestFileResults)
	if err != nil {
		t.Fatalf("GetFileIngestMark: %v", err)
	}
	if mark.Dropped != int64(over) {
		t.Errorf("dropped_count = %d, want %d", mark.Dropped, over)
	}
}

func TestReapResultsMalformedLines(t *testing.T) {
	taskDir := t.TempDir()
	writeTaskFile(t, workspace.ResultsPath(taskDir), []string{
		`{"ts":1,"axes":{"step":1},"metrics":{"m":1}}`,
		`{this is broken`,
		`{"ts":2,"axes":"not-an-object","metrics":{"m":2}}`,
		`{"ts":3,"axes":{"step":3},"metrics":[1,2]}`,
		`{"ts":4}`,
		`{"ts":5,"axes":{"step":5},"metrics":{"m":5}}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	// Line 1, the ts-only line (absent axes/metrics tolerate to {}), line 6.
	if res.ResultCount != 3 {
		t.Errorf("ResultCount = %d, want 3 (broken/wrong-typed lines skipped)", res.ResultCount)
	}
	rows, _ := st.ListResultRecords(context.Background(), "j1")
	if len(rows) != 3 {
		t.Fatalf("row count = %d, want 3: %+v", len(rows), rows)
	}
	if rows[1].AxesJSON != "{}" || rows[1].MetricsJSON != "{}" {
		t.Errorf("ts-only line should normalize to empty objects: %+v", rows[1])
	}
}

func TestReapEventsCheckpoints(t *testing.T) {
	taskDir := t.TempDir()
	writeTaskFile(t, workspace.EventsPath(taskDir), []string{
		`{"type":"checkpoint","path":"/ckpt-1","size_bytes":1024,"step":100,"is_best":false,"ts":1}`,
		`{"type":"preempted","step":101,"ts":2}`,
		`{"type":"loop_break","name":"range","reason":"plateau","ts":3}`,
		`{"type":"checkpoint","path":"/ckpt-2","size_bytes":2048,"step":200,"is_best":true,"ts":4}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.CheckpointCount != 2 {
		t.Errorf("CheckpointCount = %d, want 2 (preempted/loop_break are flight-recorder only)", res.CheckpointCount)
	}
	ckpts, _ := st.ListCheckpoints(context.Background(), "t1")
	if len(ckpts) != 2 {
		t.Fatalf("checkpoint rows = %d, want 2: %+v", len(ckpts), ckpts)
	}
	if !ckpts[1].IsBest || ckpts[1].SizeBytes != 2048 {
		t.Errorf("checkpoint fields mismatch: %+v", ckpts[1])
	}
}

func TestReapEventsStrayMetricNotSummarized(t *testing.T) {
	// A metric event in events.jsonl is a contract violation; summarizing it
	// could double-count against metrics.jsonl. It must be skipped.
	taskDir := t.TempDir()
	writeTaskFile(t, workspace.EventsPath(taskDir), []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.MetricCount != 0 {
		t.Errorf("MetricCount = %d, want 0", res.MetricCount)
	}
	sums, _ := st.ListMetricSummaries(context.Background(), "j1", "")
	if len(sums) != 0 {
		t.Errorf("stray metric produced summaries: %+v", sums)
	}
}

func TestReapAllThreeFilesOnePass(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.42,"step":100,"ts":1}`,
	})
	writeTaskFile(t, workspace.EventsPath(taskDir), []string{
		`{"type":"checkpoint","path":"/p","size_bytes":1,"step":100,"is_best":false,"ts":2}`,
	})
	writeTaskFile(t, workspace.ResultsPath(taskDir), []string{
		`{"ts":3,"axes":{"model":"a"},"metrics":{"acc":0.9}}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.MetricCount != 1 || res.CheckpointCount != 1 || res.ResultCount != 1 {
		t.Errorf("counts = %+v, want 1/1/1", res)
	}
}

func TestReapFinalFreezesAllMarks(t *testing.T) {
	// No files at all + final pass → every mark frozen; later passes are
	// no-ops even if files appear afterwards (settled tasks stay settled).
	taskDir := t.TempDir()
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)
	target := Target{TaskID: "t1", JobID: "j1", Dir: taskDir}

	if _, err := ReapIncremental(context.Background(), st, target, true); err != nil {
		t.Fatalf("final reap: %v", err)
	}
	for _, file := range []string{store.IngestFileResults, store.IngestFileEvents} {
		mark, err := st.GetFileIngestMark(context.Background(), "t1", file)
		if err != nil {
			t.Fatalf("GetFileIngestMark(%s): %v", file, err)
		}
		if !mark.Final {
			t.Errorf("mark %s not frozen after final pass", file)
		}
	}

	writeTaskFile(t, workspace.ResultsPath(taskDir), []string{
		`{"ts":1,"axes":{},"metrics":{"m":1}}`,
	})
	res, err := ReapIncremental(context.Background(), st, target, false)
	if err != nil {
		t.Fatalf("post-freeze reap: %v", err)
	}
	if res.ResultCount != 0 {
		t.Errorf("frozen task ingested new rows: %+v", res)
	}
}

func TestRetryUnfreezeContinuesResultsAtExistingOffset(t *testing.T) {
	// SDK streams are task-lifetime append logs. A retry unfreezes the final
	// mark at its existing offset, preserving the prior attempt and ingesting
	// only bytes appended by the next attempt.
	taskDir := t.TempDir()
	writeTaskFile(t, workspace.ResultsPath(taskDir), []string{
		`{"ts":1,"axes":{"step":1},"metrics":{"m":1}}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)
	target := Target{TaskID: "t1", JobID: "j1", Dir: taskDir}

	if _, err := ReapIncremental(context.Background(), st, target, true); err != nil {
		t.Fatalf("terminal reap: %v", err)
	}
	if err := st.UpdateTaskStatus(context.Background(), "t1", "failed", map[string]any{
		"status_source": "wrapper",
	}); err != nil {
		t.Fatalf("make task retryable: %v", err)
	}
	if err := st.BeginTaskRetry(context.Background(), "t1", 0, "gen-next"); err != nil {
		t.Fatalf("begin retry: %v", err)
	}
	writeTaskFile(t, workspace.ResultsPath(taskDir), []string{
		`{"ts":1,"axes":{"step":1},"metrics":{"m":1}}`,
		`{"ts":2,"axes":{"step":2},"metrics":{"m":2}}`,
	})
	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("next-attempt reap: %v", err)
	}
	rows, _ := st.ListResultRecords(context.Background(), "j1")
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2 task-lifetime records", len(rows))
	}
	if rows[0].TS != 1 || rows[1].TS != 2 {
		t.Fatalf("result history = %+v, want old then newly appended record", rows)
	}
}
