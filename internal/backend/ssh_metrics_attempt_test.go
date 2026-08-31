package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/workspace"
)

func TestSSHTaskMetricsReadIsPure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	be := newFreshnessSSHBackend(t, st, "submit {{run_sh}}")
	taskDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(taskDir, "metrics.jsonl"), []byte(
		"{\"type\":\"metric\",\"key\":\"loss\",\"value\":0.5,\"step\":1,\"ts\":10}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "p", ConfigJSON: "{}", Status: "running",
		TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "p", Command: "true", ParamsJSON: "{}",
		Status: "running", TaskDir: taskDir, Target: "hpc",
		TargetGeneration: be.Generation(), ExternalID: "external-1", EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	points, err := be.TaskMetrics(ctx, "t1", 0)
	if err != nil {
		t.Fatalf("TaskMetrics: %v", err)
	}
	if len(points) != 1 || points[0].Key != "loss" || points[0].Value != 0.5 {
		t.Fatalf("points = %+v", points)
	}
	mark, err := st.GetIngestMark(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if mark != (store.IngestMark{}) {
		t.Fatalf("read path mutated ingest mark: %+v", mark)
	}
	summaries, err := st.ListMetricSummaries(ctx, "j1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("read path mutated summaries: %+v", summaries)
	}
}

func TestSSHTaskMetricBucketsIgnorePriorAttemptPyramid(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	be := newFreshnessSSHBackend(t, st, "submit {{run_sh}}")
	taskDir := t.TempDir()
	metricsPath := filepath.Join(taskDir, "metrics.jsonl")
	first := "{\"type\":\"metric\",\"key\":\"loss\",\"value\":1,\"step\":1,\"ts\":10}\n"
	second := "{\"type\":\"metric\",\"key\":\"loss\",\"value\":2,\"step\":2,\"ts\":20}\n"
	if err := os.WriteFile(metricsPath, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.BuildPyramid(ctx, nil, taskDir); err != nil {
		t.Fatalf("build prior-attempt pyramid: %v", err)
	}
	if err := os.WriteFile(metricsPath, []byte(first+second), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j-retry", ProjectName: "p", ConfigJSON: "{}", Status: "running",
		TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "t-retry", JobID: "j-retry", ProjectName: "p", Command: "true", ParamsJSON: "{}",
		Status: "running", StatusSource: "scheduler", RetryCount: 1,
		TaskDir: taskDir, Target: "hpc", TargetGeneration: be.Generation(),
		ExternalID: "external-2", EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	buckets, source, err := be.TaskMetricBuckets(ctx, "t-retry", "loss", 0, 0, 10)
	if err != nil {
		t.Fatalf("TaskMetricBuckets: %v", err)
	}
	if source != "tail" {
		t.Fatalf("source = %q, want tail while retry is active", source)
	}
	var count int64
	for _, bucket := range buckets {
		count += bucket.Count
	}
	if count != 2 {
		t.Fatalf("bucket point count = %d, want both task-lifetime points", count)
	}
}
