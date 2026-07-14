package store

import (
	"context"
	"testing"
)

// seedTerminalJob inserts a legacy "done" job with tasks in the given
// statuses — the pre-split shape reclassifyDoneJobs must repair.
func seedTerminalJob(t *testing.T, s *Store, jobID string, taskStatuses []string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO jobs (id, project_name, config_json, status, total_tasks, created_at)
		 VALUES (?, 'p', '{}', 'done', ?, 1)`, jobID, len(taskStatuses)); err != nil {
		t.Fatalf("seed job %s: %v", jobID, err)
	}
	for i, status := range taskStatuses {
		if _, err := s.db.Exec(
			`INSERT INTO tasks (id, job_id, project_name, command, params_json, status)
			 VALUES (?, ?, 'p', 'true', '{}', ?)`,
			jobID+"-t"+string(rune('a'+i)), jobID, status); err != nil {
			t.Fatalf("seed task for %s: %v", jobID, err)
		}
	}
}

func migratedStatus(t *testing.T, s *Store, jobID string) string {
	t.Helper()
	j, err := s.GetJob(context.Background(), jobID)
	if err != nil || j == nil {
		t.Fatalf("get job %s: %v", jobID, err)
	}
	return j.Status
}

// TestReclassifyDoneJobs verifies the one-shot status-split migration: legacy
// "done" jobs are recomputed from task outcomes, and re-running Migrate is a
// no-op (idempotent).
func TestReclassifyDoneJobs(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if _, err := s.db.Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	seedTerminalJob(t, s, "j-done", []string{"success", "success"})
	seedTerminalJob(t, s, "j-killed", []string{"killed", "killed"})
	seedTerminalJob(t, s, "j-failed", []string{"failed", "killed"})
	seedTerminalJob(t, s, "j-partial", []string{"success", "failed"})
	// Dirty data guards: zero tasks / non-terminal tasks must stay "done"
	// untouched (the live aggregator owns them). "j-unknown" pins the
	// allowlist guard: an out-of-vocabulary task status (legacy/unknown)
	// must NOT be misread as terminal and reclassified.
	seedTerminalJob(t, s, "j-empty", nil)
	seedTerminalJob(t, s, "j-dirty", []string{"success", "pending"})
	seedTerminalJob(t, s, "j-unknown", []string{"success", "cancelling"})

	// The migration step runs inside Migrate (already executed by Open once,
	// before the seeds existed) — run it again against the seeded rows.
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	want := map[string]string{
		"j-done":    "done",
		"j-killed":  "killed",
		"j-failed":  "failed",
		"j-partial": "partial",
		"j-empty":   "done",
		"j-dirty":   "done",
		"j-unknown": "done",
	}
	for jobID, w := range want {
		if got := migratedStatus(t, s, jobID); got != w {
			t.Errorf("after migrate: job %s status = %q, want %q", jobID, got, w)
		}
	}

	// Idempotence: a second pass changes nothing.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	for jobID, w := range want {
		if got := migratedStatus(t, s, jobID); got != w {
			t.Errorf("after second migrate: job %s status = %q, want %q", jobID, got, w)
		}
	}
}
