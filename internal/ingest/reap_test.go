package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

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
	path := workspace.MetricsPath(taskDir)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write metrics.jsonl: %v", err)
	}
}

func TestReapNormal(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.42,"step":100,"ts":1700000000}`,
		`{"type":"checkpoint","path":"/p","size_bytes":1024,"step":100,"is_best":true,"ts":1700000001}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1", res.MetricCount)
	}
	if res.CheckpointCount != 1 {
		t.Errorf("CheckpointCount = %d, want 1", res.CheckpointCount)
	}

	got, _ := st.ListMetricSummaries(context.Background(), "j1", "loss")
	if len(got) != 1 || got[0].Min != 0.42 || got[0].Max != 0.42 || got[0].Count != 1 {
		t.Errorf("summary row mismatch: %+v", got)
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

func TestReapBrokenLine(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
		`{this is broken`,
		`{"type":"metric","key":"acc","value":0.95,"step":1,"ts":2}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.MetricCount != 2 {
		t.Errorf("MetricCount = %d, want 2 (broken line skipped)", res.MetricCount)
	}
}

func TestReapUnknownType(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
		`{"type":"image","key":"sample","path":"/img.png","ts":2}`,
		`{"type":"future_table","data":[],"ts":3}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1 (other types ignored)", res.MetricCount)
	}
}

func TestReapDiskLowIgnored(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.1,"step":1,"ts":1}`,
		`{"type":"disk_low","free_bytes":1048576,"needed_est":2097152,"ts":2}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Fatalf("ReapIncremental: %v", err)
	}
	if res.MetricCount != 1 {
		t.Errorf("MetricCount = %d, want 1 (disk_low must be ignored, metric must still land)", res.MetricCount)
	}
}

func TestReapMissingFile(t *testing.T) {
	taskDir := t.TempDir()
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)

	res, err := ReapIncremental(context.Background(), st, Target{TaskID: "t1", JobID: "j1", Dir: taskDir}, false)
	if err != nil {
		t.Errorf("missing file should be silent, got %v", err)
	}
	if res.MetricCount != 0 || res.CheckpointCount != 0 {
		t.Errorf("missing file should yield zero result, got %+v", res)
	}
}

func TestReapIdempotent(t *testing.T) {
	taskDir := t.TempDir()
	writeMetricsFile(t, taskDir, []string{
		`{"type":"metric","key":"loss","value":0.42,"step":1,"ts":1}`,
	})
	st := openMemoryStoreAndSeed(t, "t1", "j1", taskDir)
	target := Target{TaskID: "t1", JobID: "j1", Dir: taskDir}

	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("first reap: %v", err)
	}
	if _, err := ReapIncremental(context.Background(), st, target, false); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	got, _ := st.ListMetricSummaries(context.Background(), "j1", "loss")
	if len(got) != 1 || got[0].Count != 1 {
		t.Errorf("double-reap must not double-count (the (size,offset) mark makes pass 2 a zero-transfer no-op): %+v", got)
	}
}

func TestMetricsPathComesFromWorkspaceContract(t *testing.T) {
	taskDir := filepath.Join(t.TempDir(), "task")
	if got := workspace.MetricsPath(taskDir); got != filepath.Join(taskDir, "metrics.jsonl") {
		t.Fatalf("workspace.MetricsPath = %q", got)
	}
}
