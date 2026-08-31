package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

// newStatusHarness builds a minimal scheduler around an in-memory store —
// just enough to exercise RefreshJobStatus (launcher/pool are never hit).
func newStatusHarness(t *testing.T) (*Scheduler, *store.Store) {
	t.Helper()
	st := testStore(t)
	s := New(DefaultConfig(), NewQueue(), testPool(1), &fakeRemoteLauncher{}, st, testLogger())
	return s, st
}

// seedTaskWithStatus inserts a task row already settled in the given status.
func seedTaskWithStatus(t *testing.T, s *store.Store, jobID, taskID, status string) {
	t.Helper()
	err := s.InsertTask(context.Background(), &store.TaskRow{
		ID: taskID, JobID: jobID, ProjectName: "test",
		Command: "true", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "pending", EnqueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed task %s: %v", taskID, err)
	}
	if status != "pending" {
		if _, err := s.DB().Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, taskID); err != nil {
			t.Fatalf("set task %s status: %v", taskID, err)
		}
	}
}

func jobStatus(t *testing.T, s *store.Store, jobID string) string {
	t.Helper()
	j, err := s.GetJob(context.Background(), jobID)
	if err != nil || j == nil {
		t.Fatalf("get job %s: %v", jobID, err)
	}
	return j.Status
}

// TestRefreshJobStatusTerminalSplit covers the four terminal branches of the
// job aggregate: done / killed / failed / partial.
func TestRefreshJobStatusTerminalSplit(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"all success → done", []string{"success", "success"}, "done"},
		{"all killed → killed", []string{"killed", "killed"}, "killed"},
		{"all failed → failed", []string{"failed", "failed"}, "failed"},
		{"failed+killed mix → failed", []string{"failed", "killed"}, "failed"},
		{"success+failed → partial", []string{"success", "failed"}, "partial"},
		{"success+killed → partial", []string{"success", "killed"}, "partial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, st := newStatusHarness(t)
			const jobID = "j1" // fresh in-memory store per subtest
			seedJob(t, st, jobID, "test", len(tc.statuses))
			for i, status := range tc.statuses {
				seedTaskWithStatus(t, st, jobID, jobID+"-t"+string(rune('a'+i)), status)
			}
			s.RefreshJobStatus(jobID)
			if got := jobStatus(t, st, jobID); got != tc.want {
				t.Fatalf("job status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRefreshJobStatusNonTerminal verifies the live states are untouched by
// the terminal split: pending work keeps the job out of terminal statuses.
func TestRefreshJobStatusNonTerminal(t *testing.T) {
	s, st := newStatusHarness(t)

	// Some terminal, some pending, none running → "running" (started).
	seedJob(t, st, "j-live", "test", 2)
	seedTaskWithStatus(t, st, "j-live", "t-live-1", "failed")
	seedTaskWithStatus(t, st, "j-live", "t-live-2", "pending")
	s.RefreshJobStatus("j-live")
	if got := jobStatus(t, st, "j-live"); got != "running" {
		t.Fatalf("job status = %q, want running", got)
	}

	// Nothing started → "pending".
	seedJob(t, st, "j-fresh", "test", 1)
	seedTaskWithStatus(t, st, "j-fresh", "t-fresh-1", "pending")
	s.RefreshJobStatus("j-fresh")
	if got := jobStatus(t, st, "j-fresh"); got != "pending" {
		t.Fatalf("job status = %q, want pending", got)
	}
}

// TestRefreshJobStatusPausedIsSticky pins the control-state grammar: pause is
// human intent and survives task terminality. Only resume or kill (both of
// which clear the pause set) release the job into its terminal aggregate.
func TestRefreshJobStatusPausedIsSticky(t *testing.T) {
	setPaused := func(t *testing.T, st *store.Store, jobID string) {
		t.Helper()
		if _, err := st.DB().Exec(`UPDATE jobs SET status = 'paused' WHERE id = ?`, jobID); err != nil {
			t.Fatalf("set job %s paused: %v", jobID, err)
		}
	}

	t.Run("all tasks terminal → job stays paused", func(t *testing.T) {
		s, st := newStatusHarness(t)
		seedJob(t, st, "j-p", "test", 2)
		seedTaskWithStatus(t, st, "j-p", "t-p-1", "success")
		seedTaskWithStatus(t, st, "j-p", "t-p-2", "success")
		s.PauseJob("j-p")
		setPaused(t, st, "j-p")

		s.RefreshJobStatus("j-p") // the last task's lifecycle event
		if got := jobStatus(t, st, "j-p"); got != "paused" {
			t.Fatalf("job status = %q, want paused (terminality must not clobber pause)", got)
		}
	})

	t.Run("resume releases the terminal aggregate", func(t *testing.T) {
		s, st := newStatusHarness(t)
		seedJob(t, st, "j-r", "test", 2)
		seedTaskWithStatus(t, st, "j-r", "t-r-1", "success")
		seedTaskWithStatus(t, st, "j-r", "t-r-2", "failed")
		s.PauseJob("j-r")
		setPaused(t, st, "j-r")

		s.ResumeJob("j-r") // clears the pause set (backend then re-aggregates)
		s.RefreshJobStatus("j-r")
		if got := jobStatus(t, st, "j-r"); got != "partial" {
			t.Fatalf("job status after resume = %q, want partial", got)
		}
	})

	t.Run("kill path (ClearPause) releases the terminal aggregate", func(t *testing.T) {
		s, st := newStatusHarness(t)
		seedJob(t, st, "j-k", "test", 2)
		seedTaskWithStatus(t, st, "j-k", "t-k-1", "killed")
		seedTaskWithStatus(t, st, "j-k", "t-k-2", "killed")
		s.PauseJob("j-k")
		setPaused(t, st, "j-k")

		s.ClearPause("j-k") // what killJob does before its final refresh
		s.RefreshJobStatus("j-k")
		if got := jobStatus(t, st, "j-k"); got != "killed" {
			t.Fatalf("job status after kill = %q, want killed", got)
		}
	})
}
