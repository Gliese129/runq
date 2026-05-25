package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────

func openMemoryStoreAndSeed(t *testing.T, taskID, jobID, taskDir string) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: jobID, ProjectName: "p", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: taskID, JobID: jobID, ProjectName: "p",
		Command: "echo hi", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "pending", EnqueuedAt: time.Now(),
		TaskDir: taskDir,
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return st
}

func writeMetricsFile(t *testing.T, taskDir string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir taskDir: %v", err)
	}
	path := filepath.Join(taskDir, "metrics.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write metrics.jsonl: %v", err)
	}
}

// ── tests ─────────────────────────────────────────────────────────────────

// TestReapNormal: well-formed metrics.jsonl with one metric and one
// checkpoint event — both should land in their tables and the counts in
// ReapResult should match.
func TestReapNormal(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.42,"step":100,"ts":1700000000}`,
		`{"type":"checkpoint","path":"/p","size_bytes":1024,"step":100,"is_best":true,"ts":1700000001}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapTaskOutputs(context.Background(), st, &Task{ID: "t1", JobID: "j1", TaskDir: taskDir})
	if err != nil {
		t.Fatalf("ReapTaskOutputs: %v", err)
	}
	if res.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1", res.MetricCount)
	}
	if res.CheckpointCount != 1 {
		t.Errorf("CheckpointCount = %d, want 1", res.CheckpointCount)
	}

	got, _ := st.ListMetrics(context.Background(), "t1", "loss")
	if len(got) != 1 || got[0].Value != 0.42 {
		t.Errorf("metrics row mismatch: %+v", got)
	}
	ckpts, _ := st.ListCheckpoints(context.Background(), "t1")
	if len(ckpts) != 1 {
		t.Fatalf("checkpoint row count = %d, want 1: %+v", len(ckpts), ckpts)
	}
	if !ckpts[0].IsBest {
		t.Errorf("checkpoint is_best = false, want true: %+v", ckpts[0])
	}
	if ckpts[0].SizeBytes != 1024 {
		t.Errorf("checkpoint size_bytes = %d, want 1024 (verifies size_bytes JSON tag)", ckpts[0].SizeBytes)
	}
	if ckpts[0].TaskID != "t1" || ckpts[0].JobID != "j1" {
		t.Errorf("checkpoint task/job not filled from context: %+v", ckpts[0])
	}
}

// TestReapBrokenLine: one malformed JSON line in the middle. Reap must not
// abort — surrounding well-formed events still insert.
func TestReapBrokenLine(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
		`{this is broken`,
		`{"type":"metric","key":"acc","value":0.95,"step":1,"ts":2}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapTaskOutputs(context.Background(), st, &Task{ID: "t1", JobID: "j1", TaskDir: taskDir})
	if err != nil {
		t.Fatalf("ReapTaskOutputs: %v", err)
	}
	if res.MetricCount != 2 {
		t.Errorf("MetricCount = %d, want 2 (broken line skipped)", res.MetricCount)
	}
}

// TestReapUnknownType: forward-compat for future SDK event types — daemon
// should silently skip unknown `type` values, not error out.
func TestReapUnknownType(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
		`{"type":"image","key":"sample","path":"/img.png","ts":2}`,
		`{"type":"future_table","data":[],"ts":3}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapTaskOutputs(context.Background(), st, &Task{ID: "t1", JobID: "j1", TaskDir: taskDir})
	if err != nil {
		t.Fatalf("ReapTaskOutputs: %v", err)
	}
	if res.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1 (other types ignored)", res.MetricCount)
	}
}

// TestReapDiskLowIgnored: post-pivot, disk_low goes through the SDK's HTTP
// channel, not metrics.jsonl. If an old jsonl from a pre-pivot task still
// contains a disk_low line, reap must silently skip it (forward-compat
// "unknown type" path) — never crash, never populate any flag.
func TestReapDiskLowIgnored(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
		`{"type":"disk_low","free_bytes":1048576,"needed_est":2097152,"ts":2}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapTaskOutputs(context.Background(), st, &Task{ID: "t1", JobID: "j1", TaskDir: taskDir})
	if err != nil {
		t.Fatalf("ReapTaskOutputs: %v", err)
	}
	if res.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1 (disk_low must be ignored, metric must still land)", res.MetricCount)
	}
}

// TestReapMissingFile: tasks that don't log any metric still trigger reap;
// the absence of metrics.jsonl must not be treated as an error.
func TestReapMissingFile(t *testing.T) {
	taskDir := t.TempDir() // no file inside
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapTaskOutputs(context.Background(), st, &Task{ID: "t1", JobID: "j1", TaskDir: taskDir})
	if err != nil {
		t.Errorf("missing file should be silent, got %v", err)
	}
	if res.MetricCount != 0 || res.CheckpointCount != 0 {
		t.Errorf("missing file should yield zero result, got %+v", res)
	}
}

// TestReapIdempotent: re-running reap on the same metrics.jsonl (matches
// the daemon-restart-then-reclaim path) must not duplicate rows.
func TestReapIdempotent(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.42,"step":1,"ts":1}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)
	task := &Task{ID: "t1", JobID: "j1", TaskDir: taskDir}

	if _, err := ReapTaskOutputs(context.Background(), st, task); err != nil {
		t.Fatalf("first reap: %v", err)
	}
	if _, err := ReapTaskOutputs(context.Background(), st, task); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM metrics WHERE task_id='t1'`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 row after double-reap (INSERT OR IGNORE), got %d", n)
	}
}
