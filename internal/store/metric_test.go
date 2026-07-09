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

func TestMergeMetricSummaries(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	// First delta pass.
	if err := s.MergeMetricSummaries(context.Background(), []MetricSummaryRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Min: 0.4, Max: 0.9, Last: 0.5, LastTS: 100, Count: 10},
		{TaskID: "t1", JobID: "j1", Key: "lr", Min: 1e-4, Max: 1e-4, Last: 1e-4, LastTS: 100, Count: 10},
	}); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	// Second delta: lower min, later last — must compose losslessly.
	if err := s.MergeMetricSummaries(context.Background(), []MetricSummaryRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Min: 0.2, Max: 0.3, Last: 0.2, LastTS: 200, Count: 5},
	}); err != nil {
		t.Fatalf("merge 2: %v", err)
	}

	got, err := s.ListMetricSummaries(context.Background(), "j1", "loss")
	if err != nil || len(got) != 1 {
		t.Fatalf("ListMetricSummaries: %v (%d rows)", err, len(got))
	}
	sm := got[0]
	if sm.Min != 0.2 || sm.Max != 0.9 || sm.Count != 15 || sm.Last != 0.2 || sm.LastTS != 200 {
		t.Errorf("merged summary = %+v", sm)
	}

	// Out-of-order delta (older last_ts) must NOT clobber last.
	if err := s.MergeMetricSummaries(context.Background(), []MetricSummaryRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Min: 0.25, Max: 0.25, Last: 0.25, LastTS: 150, Count: 1},
	}); err != nil {
		t.Fatalf("merge 3: %v", err)
	}
	got, _ = s.ListMetricSummaries(context.Background(), "j1", "loss")
	if got[0].Last != 0.2 || got[0].LastTS != 200 || got[0].Count != 16 {
		t.Errorf("out-of-order merge clobbered last: %+v", got[0])
	}
}

func TestMergeMetricSummariesEmpty(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	if err := s.MergeMetricSummaries(context.Background(), nil); err != nil {
		t.Errorf("empty slice should be a no-op, got %v", err)
	}
}

func TestMetricKeysDiscovery(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	if err := s.MergeMetricSummaries(context.Background(), []MetricSummaryRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Min: 1, Max: 1, Last: 1, LastTS: 1, Count: 1},
		{TaskID: "t1", JobID: "j1", Key: "acc", Min: 1, Max: 1, Last: 1, LastTS: 1, Count: 1},
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	keys, err := s.MetricKeys(context.Background(), "j1")
	if err != nil {
		t.Fatalf("MetricKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "acc" || keys[1] != "loss" {
		t.Errorf("keys = %v, want [acc loss]", keys)
	}
}

func TestIngestMarkRoundTrip(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	// Absent → zero value.
	m, err := s.GetIngestMark(context.Background(), "t1")
	if err != nil || m.Size != 0 || m.Final {
		t.Fatalf("zero mark: %+v, %v", m, err)
	}
	if err := s.SetIngestMark(context.Background(), "t1", IngestMark{Size: 100, Offset: 90, Final: false}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetIngestMark(context.Background(), "t1", IngestMark{Size: 200, Offset: 200, Final: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.InsertCheckpointsBatch(context.Background(), []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p/ckpt.pt", SizeBytes: 1024, Step: ptrInt64(1), TS: 1700000000},
	}); err != nil {
		t.Fatalf("checkpoint seed: %v", err)
	}
	m, _ = s.GetIngestMark(context.Background(), "t1")
	if m.Size != 200 || m.Offset != 200 || !m.Final {
		t.Errorf("mark = %+v", m)
	}
	// DeleteTaskMetrics unfreezes (retry path). checkpoints survive: they
	// are a task-lifetime log (freeze sizing, resume source), NOT a
	// projection of the current metrics.jsonl.
	if err := s.DeleteTaskMetrics(context.Background(), "t1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	m, _ = s.GetIngestMark(context.Background(), "t1")
	if m.Final || m.Size != 0 {
		t.Errorf("mark after delete = %+v", m)
	}
	var ckptCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM checkpoints WHERE task_id='t1'`).Scan(&ckptCount); err != nil {
		t.Fatal(err)
	}
	if ckptCount != 1 {
		t.Errorf("checkpoints must SURVIVE retry unfreeze (task-lifetime log), got %d rows", ckptCount)
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

func TestInsertCheckpointsBatchNewerTSWins(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	step := ptrInt64(1)
	if err := s.InsertCheckpointsBatch(context.Background(), []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p/old.pt", SizeBytes: 1024, Step: step, IsBest: false, TS: 100},
	}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if err := s.InsertCheckpointsBatch(context.Background(), []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p/new.pt", SizeBytes: 2048, Step: step, IsBest: true, TS: 200},
	}); err != nil {
		t.Fatalf("insert newer: %v", err)
	}
	if err := s.InsertCheckpointsBatch(context.Background(), []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p/stale.pt", SizeBytes: 4096, Step: step, IsBest: false, TS: 150},
	}); err != nil {
		t.Fatalf("insert stale: %v", err)
	}

	got, err := s.ListCheckpoints(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one checkpoint row after upserts, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/p/new.pt" || got[0].SizeBytes != 2048 || !got[0].IsBest || got[0].TS != 200 {
		t.Errorf("newer checkpoint did not win or stale row clobbered it: %+v", got[0])
	}
}

func TestMetricsCascadeDelete(t *testing.T) {
	s, _ := Open(":memory:")
	defer s.Close()
	seedJobAndTask(t, s, "t1", "j1")

	if err := s.MergeMetricSummaries(context.Background(), []MetricSummaryRow{
		{TaskID: "t1", JobID: "j1", Key: "loss", Min: 1, Max: 1, Last: 1, LastTS: 1, Count: 1},
	}); err != nil {
		t.Fatalf("merge summary: %v", err)
	}
	if err := s.SetIngestMark(context.Background(), "t1", IngestMark{Size: 10, Offset: 10}); err != nil {
		t.Fatalf("set mark: %v", err)
	}
	if err := s.InsertCheckpointsBatch(context.Background(), []CheckpointRow{
		{TaskID: "t1", JobID: "j1", Path: "/p", Step: ptrInt64(1), TS: 1},
	}); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	if err := s.DeleteTask(context.Background(), "t1"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	var sc, ic, cc int
	s.DB().QueryRow(`SELECT COUNT(*) FROM metric_summary WHERE task_id='t1'`).Scan(&sc)
	s.DB().QueryRow(`SELECT COUNT(*) FROM metrics_ingest WHERE task_id='t1'`).Scan(&ic)
	s.DB().QueryRow(`SELECT COUNT(*) FROM checkpoints WHERE task_id='t1'`).Scan(&cc)
	if sc != 0 || ic != 0 || cc != 0 {
		t.Errorf("ON DELETE CASCADE should have removed dependents; got summaries=%d, marks=%d, checkpoints=%d", sc, ic, cc)
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
