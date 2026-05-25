package store

import (
	"context"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────

// seedJobAndTask creates a project + job + task so foreign-key constraints
// don't trip up metric/checkpoint inserts. Returns nothing because callers
// always reuse the same IDs.
func seedJobAndTask(t *testing.T, s *Store, taskID, jobID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('proj', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := s.InsertJob(ctx, &JobRow{
		ID: jobID, ProjectName: "proj", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := s.InsertTask(ctx, &TaskRow{
		ID: taskID, JobID: jobID, ProjectName: "proj",
		Command: "echo hi", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "pending", EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func ptrInt64(v int64) *int64 { return &v }

// ── tests ─────────────────────────────────────────────────────────────────

func TestInsertMetricsBatch(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	rows := []MetricRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Value: 0.42, Step: ptrInt64(100), TS: 1700000000},
		{TaskID: "t1", JobID: "j1", Key: "lr", Value: 1e-4, Step: ptrInt64(100), TS: 1700000000},
	}
	if err := s.InsertMetricsBatch(context.Background(), rows); err != nil {
		t.Fatalf("InsertMetricsBatch: %v", err)
	}

	var count int
	s.DB().QueryRow(`SELECT COUNT(*) FROM metrics WHERE task_id = ?`, "t1").Scan(&count)
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}

	got, err := s.ListMetrics(context.Background(), "t1", "loss")
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(got) != 1 || got[0].Value != 0.42 {
		t.Errorf("ListMetrics(loss) = %+v, want one row with value 0.42", got)
	}
	if got[0].Step == nil || *got[0].Step != 100 {
		t.Errorf("expected step 100, got %v", got[0].Step)
	}
}

func TestInsertMetricsBatchNullStep(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	rows := []MetricRow{
		{TaskID: "t1", JobID: "j1", Key: "no_step", Value: 1.0, Step: nil, TS: 1700000000},
		// Different ts so PK (task_id, key, step, ts) doesn't collide on null step.
		{TaskID: "t1", JobID: "j1", Key: "no_step", Value: 2.0, Step: nil, TS: 1700000001},
	}
	if err := s.InsertMetricsBatch(context.Background(), rows); err != nil {
		t.Fatalf("InsertMetricsBatch: %v", err)
	}

	got, _ := s.ListMetrics(context.Background(), "t1", "no_step")
	if len(got) != 2 {
		t.Errorf("nullable step + distinct ts should produce 2 rows, got %d", len(got))
	}
	for _, m := range got {
		if m.Step != nil {
			t.Errorf("expected null step, got %v", *m.Step)
		}
	}
}

func TestInsertMetricsBatchEmpty(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	if err := s.InsertMetricsBatch(context.Background(), nil); err != nil {
		t.Errorf("empty slice should be a no-op, got %v", err)
	}
}

func TestInsertMetricsBatchIdempotent(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	rows := []MetricRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Value: 0.42, Step: ptrInt64(1), TS: 1700000000},
	}
	if err := s.InsertMetricsBatch(context.Background(), rows); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Re-reaping the same jsonl after daemon restart should not error out
	// or duplicate rows.
	if err := s.InsertMetricsBatch(context.Background(), rows); err != nil {
		t.Fatalf("second insert (idempotent): %v", err)
	}
	var count int
	s.DB().QueryRow(`SELECT COUNT(*) FROM metrics WHERE task_id='t1'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row after dup insert (INSERT OR IGNORE), got %d", count)
	}
}

func TestInsertCheckpointsBatch(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	rows := []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p/ckpt_1.pt", SizeBytes: 1024, Step: ptrInt64(1), IsBest: false, TS: 1700000000},
		{TaskID: "t1", JobID: "j1", Path: "/p/ckpt_best.pt", SizeBytes: 2048, Step: ptrInt64(2), IsBest: true, TS: 1700000001},
	}
	if err := s.InsertCheckpointsBatch(context.Background(), rows); err != nil {
		t.Fatalf("InsertCheckpointsBatch: %v", err)
	}

	got, err := s.ListCheckpoints(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if !got[1].IsBest || got[0].IsBest {
		t.Errorf("is_best mapped wrong: %+v", got)
	}
}

func TestMetricsCascadeDelete(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	if err := s.InsertMetricsBatch(context.Background(), []MetricRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Value: 1, Step: ptrInt64(1), TS: 1},
	}); err != nil {
		t.Fatalf("insert metric: %v", err)
	}
	if err := s.InsertCheckpointsBatch(context.Background(), []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p", Step: ptrInt64(1), TS: 1},
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	if err := s.DeleteTask(context.Background(), "t1"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	var mc, cc int
	s.DB().QueryRow(`SELECT COUNT(*) FROM metrics WHERE task_id='t1'`).Scan(&mc)
	s.DB().QueryRow(`SELECT COUNT(*) FROM checkpoints WHERE task_id='t1'`).Scan(&cc)
	if mc != 0 || cc != 0 {
		t.Errorf("ON DELETE CASCADE should have removed dependents; got metrics=%d, checkpoints=%d", mc, cc)
	}
}

func TestMaxCheckpointSize(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	// Empty history → 0, no error.
	got, err := s.MaxCheckpointSize(context.Background(), "t1")
	if err != nil {
		t.Fatalf("MaxCheckpointSize empty: %v", err)
	}
	if got != 0 {
		t.Errorf("empty history should return 0, got %d", got)
	}

	// Insert three checkpoints, verify max wins (not last).
	rows := []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/a", SizeBytes: 1000, Step: ptrInt64(1), TS: 1},
		{TaskID: "t1", JobID: "j1", Path: "/b", SizeBytes: 5000, Step: ptrInt64(2), TS: 2},
		{TaskID: "t1", JobID: "j1", Path: "/c", SizeBytes: 3000, Step: ptrInt64(3), TS: 3},
	}
	if err := s.InsertCheckpointsBatch(context.Background(), rows); err != nil {
		t.Fatalf("InsertCheckpointsBatch: %v", err)
	}

	got, err = s.MaxCheckpointSize(context.Background(), "t1")
	if err != nil {
		t.Fatalf("MaxCheckpointSize: %v", err)
	}
	if got != 5000 {
		t.Errorf("expected 5000 (max of {1000, 5000, 3000}), got %d", got)
	}
}

func TestMaxCheckpointSizeUnknownTask(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	got, err := s.MaxCheckpointSize(context.Background(), "nonexistent")
	if err != nil {
		t.Errorf("unknown task should return (0, nil), got err=%v", err)
	}
	if got != 0 {
		t.Errorf("unknown task should return 0, got %d", got)
	}
}

func TestTaskDirColumnExists(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	rows, err := s.DB().Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "task_dir" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tasks.task_dir column missing")
	}
}

func TestTaskDirRoundTrip(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()

	if _, err := s.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	ctx := context.Background()
	if err := s.InsertJob(ctx, &JobRow{
		ID: "j1", ProjectName: "p", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	want := "/work/.runq/abc"
	if err := s.InsertTask(ctx, &TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "p",
		Command: "echo hi", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "pending", EnqueuedAt: time.Now(),
		TaskDir: want,
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.TaskDir != want {
		t.Errorf("TaskDir round-trip: got %q, want %q", got.TaskDir, want)
	}
}
