package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// RQ-74: an interrupted/untracked launch marks the task `unknown` — queue and
// DB agree, the evidence is persisted, and the task is NOT requeued (double
// submission is the failure mode this state exists to prevent).
func TestLaunchUntrackedMarksUnknown(t *testing.T) {
	cause := fmt.Errorf("%w: submit command interrupted mid-flight", ErrLaunchUntracked)
	fl := &fakeRemoteLauncher{launchErr: cause}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-unknown")

	s.launchAsync(task)

	if got := taskStatus(t, s, task.ID); got != "unknown" {
		t.Fatalf("db status = %q, want unknown", got)
	}
	if qt := q.Get(task.ID); qt == nil || qt.Status != StatusUnknown {
		t.Fatalf("queue status = %v, want unknown", qt)
	}
	row, _ := s.store.GetTask(context.Background(), task.ID)
	if !strings.Contains(row.FailureDetail, "interrupted mid-flight") {
		t.Errorf("failure_detail missing cause: %q", row.FailureDetail)
	}
	if row.StatusSource != "submit" {
		t.Errorf("status_source = %q, want submit", row.StatusSource)
	}
}

// A reconcile verdict must be able to settle an unknown task through the
// FinishTask funnel (the staleness gate accepts unknown), and the terminal
// write clears the stale submit evidence.
func TestUnknownSettledByVerdict(t *testing.T) {
	cause := fmt.Errorf("%w: id lost", ErrLaunchUntracked)
	fl := &fakeRemoteLauncher{launchErr: cause}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-unknown-heal")
	s.launchAsync(task)

	// Reconcile found the wrapper's success marker.
	s.FinishTask(task, StatusSuccess, map[string]any{"status_source": "wrapper"})

	if got := taskStatus(t, s, task.ID); got != "success" {
		t.Fatalf("db status = %q, want success", got)
	}
	row, _ := s.store.GetTask(context.Background(), task.ID)
	if row.FailureDetail != "" {
		t.Errorf("stale submit evidence survived into terminal: %q", row.FailureDetail)
	}
}

// A transient launch failure keeps the task pending but leaves a visible,
// deduplicated note; a successful later handoff is expected to clear it via
// the launcher (not simulated here — the fake bypasses the DB write, so this
// test only pins the note itself).
func TestTransientLaunchLeavesVisibleNote(t *testing.T) {
	cause := fmt.Errorf("%w: dial tcp: i/o timeout", ErrLaunchTransient)
	fl := &fakeRemoteLauncher{launchErr: cause}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-transient-note")

	s.launchAsync(task)

	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("db status = %q, want pending (transient must requeue)", got)
	}
	row, _ := s.store.GetTask(context.Background(), task.ID)
	if !strings.Contains(row.FailureDetail, "i/o timeout") ||
		!strings.Contains(row.FailureDetail, "retry") {
		t.Errorf("transient note missing/wrong: %q", row.FailureDetail)
	}

	// Same failure again: the dedup map must swallow the identical write
	// (no error, still pending, note unchanged).
	task.Status = StatusRunning // simulate re-dispatch
	s.launchAsync(task)
	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("db status after second transient = %q, want pending", got)
	}
}

// RQ-74 review finding 1: reconcile confirming a live job flips the queue
// entry unknown → running, and the resumed task remains fully killable /
// finishable through the normal lifecycle.
func TestResumeUnknownFlipsQueueToRunning(t *testing.T) {
	cause := fmt.Errorf("%w: verdict lost", ErrLaunchUntracked)
	fl := &fakeRemoteLauncher{launchErr: cause}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-unknown-resume")
	s.launchAsync(task)

	if qt := q.Get(task.ID); qt == nil || qt.Status != StatusUnknown {
		t.Fatalf("precondition: queue status = %v, want unknown", qt)
	}
	s.ResumeUnknown(task.ID)
	if qt := q.Get(task.ID); qt == nil || qt.Status != StatusRunning {
		t.Fatalf("queue status after resume = %v, want running", qt)
	}

	// Resumed task goes through the normal verdict funnel.
	s.FinishTask(task, StatusSuccess, map[string]any{"status_source": "wrapper"})
	if got := taskStatus(t, s, task.ID); got != "success" {
		t.Fatalf("db status = %q, want success", got)
	}

	// Resuming a non-unknown task is a no-op, not an error path.
	s.ResumeUnknown(task.ID)
}

// Sanity: the rejected path (plain error) still fails permanently with
// evidence — unknown must not swallow deterministic rejections.
func TestRejectedStillFailsPermanently(t *testing.T) {
	fl := &fakeRemoteLauncher{launchErr: errors.New("submit t rejected (exit 255):\nqsub: invalid group")}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-rejected")
	task.MaxRetry = 0

	s.launchAsync(task)

	if got := taskStatus(t, s, task.ID); got != "failed" {
		t.Fatalf("db status = %q, want failed", got)
	}
	row, _ := s.store.GetTask(context.Background(), task.ID)
	if !strings.Contains(row.FailureDetail, "invalid group") {
		t.Errorf("rejection evidence missing: %q", row.FailureDetail)
	}
}
