package store

import (
	"context"
	"testing"
	"time"
)

// RQ-74: failure_detail round-trip — the submit-rejection evidence written by
// the scheduler must come back verbatim through GetTask/ListTasks, and a
// requeue-style update (failure_detail: nil) must clear it.
func TestFailureDetailRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('proj', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := s.InsertJob(ctx, &JobRow{
		ID: "j1", ProjectName: "proj", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := s.InsertTask(ctx, &TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "proj",
		Command: "echo hi", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "pending", EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	detail := "submit t1 rejected (exit 255):\nqsub: Please specify a valid group name after option -g.\n\nsubmit command (/w/j1/t1/submit.cmd):\nqsub -g '' run.sh"
	if err := s.UpdateTaskStatus(ctx, "t1", "failed", map[string]any{
		"failure_detail": detail,
		"status_source":  "submit",
	}); err != nil {
		t.Fatalf("update with failure_detail: %v", err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.FailureDetail != detail {
		t.Errorf("failure_detail round-trip mismatch:\n got: %q\nwant: %q", got.FailureDetail, detail)
	}
	if got.StatusSource != "submit" {
		t.Errorf("status_source = %q, want submit", got.StatusSource)
	}

	// Requeue clears the evidence: a new attempt starts clean.
	if err := s.UpdateTaskStatus(ctx, "t1", "pending", map[string]any{
		"failure_detail": nil,
	}); err != nil {
		t.Fatalf("clear failure_detail: %v", err)
	}
	got, err = s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task after clear: %v", err)
	}
	if got.FailureDetail != "" {
		t.Errorf("failure_detail not cleared on requeue: %q", got.FailureDetail)
	}
}
