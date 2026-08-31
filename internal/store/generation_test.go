package store

import (
	"context"
	"testing"
	"time"
)

func genTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedGenTask(t *testing.T, s *Store, id, target, gen, status, extID string) {
	t.Helper()
	jobID := "j-" + id
	_ = s.InsertJob(context.Background(), &JobRow{
		ID: jobID, ProjectName: "p", Status: "pending", TotalTasks: 1,
		CreatedAt: time.Now(), Target: target,
	})
	row := &TaskRow{
		ID: id, JobID: jobID, ProjectName: "p", Command: "true",
		ParamsJSON: "{}", GPUsNeeded: 1, Status: status,
		EnqueuedAt: time.Now(), Target: target,
		TargetGeneration: gen, ExternalID: extID,
	}
	if err := s.InsertTask(context.Background(), row); err != nil {
		t.Fatal(err)
	}
}

// The generation stamp must survive the insert→scan round trip.
func TestTargetGenerationRoundTrip(t *testing.T) {
	s := genTestStore(t)
	seedGenTask(t, s, "t1", "hpc", "gen-A", "pending", "")
	row, err := s.GetTask(context.Background(), "t1")
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.TargetGeneration != "gen-A" {
		t.Fatalf("target_generation = %q, want gen-A", row.TargetGeneration)
	}
}

func TestLaneScopePointOwnershipIsGenerationExact(t *testing.T) {
	active := NewLaneScope("hpc", "new")
	if !active.Owns("hpc", "new") || !active.Owns("hpc", "") {
		t.Fatal("active lane must own its generation and legacy rows")
	}
	if active.Owns("hpc", "old") || active.Owns("other", "new") {
		t.Fatal("active point routing accepted another generation or target")
	}
	active.MarkRetiring()
	if !active.Owns("hpc", "new") || active.Owns("hpc", "") {
		t.Fatal("retiring lane must own only its exact generation")
	}
}

func TestActiveScopeDoesNotAdoptSettledRecordedGeneration(t *testing.T) {
	s := genTestStore(t)
	ctx := context.Background()
	if err := s.UpsertRetiredGeneration(ctx, &TargetGenerationRow{
		Target: "hpc", Generation: "settled", ConfigJSON: `{}`, Reason: "changed",
		RetiredAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkGenerationDone(ctx, "hpc", "settled"); err != nil {
		t.Fatal(err)
	}
	seedGenTask(t, s, "t-own", "hpc", "active", "running", "1")
	seedGenTask(t, s, "t-orphan", "hpc", "unrecorded", "running", "2")
	seedGenTask(t, s, "t-settled", "hpc", "settled", "running", "3")
	rows, err := s.ListTasks(ctx, TaskFilter{Target: "hpc", Scope: NewLaneScope("hpc", "active")})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := range rows {
		seen[rows[i].ID] = true
	}
	if !seen["t-own"] || !seen["t-orphan"] || seen["t-settled"] {
		t.Fatalf("active scope rows = %v, want own+unrecorded and no settled generation", seen)
	}
}

func TestRetiredGenerationLifecycle(t *testing.T) {
	s := genTestStore(t)
	ctx := context.Background()
	g := &TargetGenerationRow{
		Target: "hpc", Generation: "gen-A", ConfigJSON: `{"name":"hpc"}`,
		Reason: "changed", RetiredAt: time.Now().Unix(),
	}
	if err := s.UpsertRetiredGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-retire.
	if err := s.UpsertRetiredGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}

	open, err := s.ListRetiringGenerations(ctx)
	if err != nil || len(open) != 1 || open[0].Generation != "gen-A" || open[0].DoneAt != nil {
		t.Fatalf("retiring = %+v, err %v", open, err)
	}

	// Unfinished count drives retirement: running counts, terminal doesn't.
	seedGenTask(t, s, "t-run", "hpc", "gen-A", "running", "ext1")
	seedGenTask(t, s, "t-done", "hpc", "gen-A", "success", "ext2")
	seedGenTask(t, s, "t-other", "hpc", "gen-B", "running", "ext3")
	n, err := s.CountUnfinishedGenerationTasks(ctx, "hpc", "gen-A")
	if err != nil || n != 1 {
		t.Fatalf("unfinished = %d (err %v), want 1", n, err)
	}

	if err := s.MarkGenerationDone(ctx, "hpc", "gen-A"); err != nil {
		t.Fatal(err)
	}
	open, err = s.ListRetiringGenerations(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("after done: retiring = %+v, err %v", open, err)
	}
	all, err := s.ListArchivedGenerations(ctx, "hpc")
	if err != nil || len(all) != 1 || all[0].DoneAt == nil {
		t.Fatalf("archived = %+v, err %v", all, err)
	}
}

// Same-name change: pending (unsubmitted) rows migrate to the new
// generation; in-flight rows stay with their owner.
func TestRestampPendingTasks(t *testing.T) {
	s := genTestStore(t)
	ctx := context.Background()
	seedGenTask(t, s, "t-pend", "hpc", "gen-A", "pending", "")
	seedGenTask(t, s, "t-inflight", "hpc", "gen-A", "pending", "12345") // submitted, awaiting cluster
	seedGenTask(t, s, "t-run", "hpc", "gen-A", "running", "67890")

	n, err := s.RestampPendingTasks(ctx, "hpc", "gen-B")
	if err != nil || n != 1 {
		t.Fatalf("restamped %d (err %v), want 1", n, err)
	}
	row, _ := s.GetTask(ctx, "t-pend")
	if row.TargetGeneration != "gen-B" {
		t.Fatalf("pending row not migrated: %q", row.TargetGeneration)
	}
	row, _ = s.GetTask(ctx, "t-inflight")
	if row.TargetGeneration != "gen-A" {
		t.Fatalf("in-flight row stolen from its generation: %q", row.TargetGeneration)
	}
}

// Removed target: pending rows are stopped with a visible reason;
// in-flight rows are untouched (the retiring lane tracks them).
func TestStopPendingTasks(t *testing.T) {
	s := genTestStore(t)
	ctx := context.Background()
	seedGenTask(t, s, "t-pend", "hpc", "gen-A", "pending", "")
	seedGenTask(t, s, "t-run", "hpc", "gen-A", "running", "111")

	ids, err := s.StopPendingTasks(ctx, "hpc", "target removed from config.yaml — pending task stopped before submission")
	if err != nil || len(ids) != 1 || ids[0] != "t-pend" {
		t.Fatalf("stopped = %v, err %v", ids, err)
	}
	row, _ := s.GetTask(ctx, "t-pend")
	if row.Status != "killed" || row.StatusSource != "runq" {
		t.Fatalf("stopped row: status %q source %q", row.Status, row.StatusSource)
	}
	if row.FailureDetail == "" || row.FinishedAt == nil {
		t.Fatalf("stopped row missing reason/finish: %+v", row)
	}
	row, _ = s.GetTask(ctx, "t-run")
	if row.Status != "running" {
		t.Fatalf("in-flight row was touched: %q", row.Status)
	}
}
