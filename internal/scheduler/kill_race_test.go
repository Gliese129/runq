package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Regression tests for RQ-69: a user kill must terminate a task even when
// it lands in the windows where no external cancel handle exists —
// between retry attempts (pending, external_id cleared) or while the
// remote submit is in flight (running, external_id not yet backfilled).
// Before the ownership-flag protocol this was an unkillable resubmit loop
// under max_retry=0 (unlimited).

// fakeRemoteLauncher is an unsupervised (remote-lane-shaped) launcher.
type fakeRemoteLauncher struct {
	mu        sync.Mutex
	launched  []string
	killed    []string
	gate      chan struct{} // when non-nil, Launch blocks until closed
	launchErr error         // when non-nil, Launch fails with it
	killErr   error         // when non-nil, KillErr reports it (cancel failed)
}

func (f *fakeRemoteLauncher) Launch(_ context.Context, t *Task, _ map[string]string, _ func(StartInfo)) (LaunchResult, error) {
	if f.gate != nil {
		<-f.gate
	}
	if f.launchErr != nil {
		return LaunchResult{}, f.launchErr
	}
	f.mu.Lock()
	f.launched = append(f.launched, t.ID)
	f.mu.Unlock()
	return LaunchResult{ExtID: t.ID + "-a0"}, nil
}

func (f *fakeRemoteLauncher) Kill(taskID string) { _ = f.KillErr(taskID) }

// KillErr mirrors remote.Launcher's error-reporting cancel (RQ-69).
func (f *fakeRemoteLauncher) KillErr(taskID string) error {
	f.mu.Lock()
	f.killed = append(f.killed, taskID)
	f.mu.Unlock()
	return f.killErr
}

func (f *fakeRemoteLauncher) Supervised() bool { return false }

func (f *fakeRemoteLauncher) killedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

func newKillRaceHarness(t *testing.T, launcher Launcher) (*Scheduler, *Queue, *fakeRemoteLauncher) {
	t.Helper()
	q := NewQueue()
	st := testStore(t)
	fl, _ := launcher.(*fakeRemoteLauncher)
	s := New(DefaultConfig(), q, testPool(1), launcher, st, testLogger(), nil, "", nil)
	return s, q, fl
}

func seedRunningTask(t *testing.T, s *Scheduler, q *Queue, id string) *Task {
	t.Helper()
	task := &Task{
		ID: id, JobID: "j-" + id, ProjectName: "test",
		Command: "true", GPUsNeeded: 1,
		MaxRetry: -1, // unlimited retry — the RQ-69 loop precondition
	}
	seedJob(t, s.store, task.JobID, "test", 1)
	seedTask(t, s.store, task)
	q.Push(task)
	// Simulate "handed to the remote lane": queue entry running, no
	// external id yet (exactly the in-flight window).
	task.Status = StatusRunning
	return task
}

func taskStatus(t *testing.T, s *Scheduler, id string) string {
	t.Helper()
	row, err := s.store.GetTask(context.Background(), id)
	if err != nil || row == nil {
		t.Fatalf("get task %s: %v", id, err)
	}
	return row.Status
}

// A failure verdict arriving after RequestKill must settle killed — never
// requeue. This is the "kill during pending-retry / refused remote kill"
// path: the flag persists until a lifecycle event can honor it.
func TestKillFlagSettlesFailureAsKilled(t *testing.T) {
	fl := &fakeRemoteLauncher{}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-killflag")

	s.RequestKill(task.ID)
	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})

	if got := taskStatus(t, s, task.ID); got != "killed" {
		t.Fatalf("status = %q, want killed (flag must beat retry)", got)
	}
	if qt := q.Get(task.ID); qt != nil && qt.Status == StatusPending {
		t.Fatal("task was requeued despite kill flag — unkillable loop regressed")
	}
}

// Control: without the flag, the same verdict retries (unlimited budget).
func TestFailureWithoutKillFlagStillRetries(t *testing.T) {
	fl := &fakeRemoteLauncher{}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-retries")

	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})

	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("status = %q, want pending (retry)", got)
	}
	if qt := q.Get(task.ID); qt == nil || qt.Status != StatusPending || qt.RetryCount != 1 {
		t.Fatalf("queue entry not requeued correctly: %+v", qt)
	}
}

// A kill racing an in-flight submit: the flag is set while Launch blocks;
// on submit completion the scheduler must cancel the freshly created
// remote job (late external id) and settle killed — no leaked cluster job.
func TestKillDuringInflightSubmitCancelsLateJob(t *testing.T) {
	fl := &fakeRemoteLauncher{gate: make(chan struct{})}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-inflight")

	done := make(chan struct{})
	go func() {
		s.launchAsync(task)
		close(done)
	}()

	s.RequestKill(task.ID) // kill lands while the submit is in flight
	close(fl.gate)         // submit completes, external id now exists

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("launchAsync did not return")
	}

	killed := fl.killedIDs()
	if len(killed) != 1 || killed[0] != task.ID {
		t.Fatalf("launcher.Kill calls = %v, want exactly [%s] (late job must be cancelled)", killed, task.ID)
	}
	if got := taskStatus(t, s, task.ID); got != "killed" {
		t.Fatalf("status = %q, want killed", got)
	}
}

// Kill must win on a transient launch failure: the submit never reached
// the scheduler, so settling killed locally is honest — requeueing would
// resubmit a task the user already killed.
func TestKillWinsOnTransientLaunch(t *testing.T) {
	fl := &fakeRemoteLauncher{launchErr: ErrLaunchTransient}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-transient")

	s.RequestKill(task.ID)
	s.launchAsync(task)

	if got := taskStatus(t, s, task.ID); got != "killed" {
		t.Fatalf("status = %q, want killed (kill must beat transient requeue)", got)
	}
	if qt := q.Get(task.ID); qt != nil && qt.Status == StatusPending {
		t.Fatal("task requeued despite kill flag on transient failure")
	}
}

// Kill must win on a deterministic scheduler rejection: no remote job
// exists, and the user asked for killed — not a permanent failure.
func TestKillWinsOnRejectedLaunch(t *testing.T) {
	fl := &fakeRemoteLauncher{launchErr: context.DeadlineExceeded} // any non-sentinel error = rejected
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-rejected")

	s.RequestKill(task.ID)
	s.launchAsync(task)

	if got := taskStatus(t, s, task.ID); got != "killed" {
		t.Fatalf("status = %q, want killed (kill must beat rejected→failed)", got)
	}
}

// A late cancel that FAILS must not record killed (the kill never lies):
// the flag is retained and the next lifecycle event settles the task.
func TestLateCancelFailureRetainsKillIntent(t *testing.T) {
	fl := &fakeRemoteLauncher{killErr: context.DeadlineExceeded}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-cancelfail")

	s.RequestKill(task.ID)
	s.launchAsync(task) // submit OK, late cancel attempted and fails

	if got := taskStatus(t, s, task.ID); got == "killed" {
		t.Fatal("recorded killed although the remote cancel failed — the kill lied")
	}
	if !s.killPending(task.ID) {
		t.Fatal("kill intent was consumed despite cancel failure")
	}

	// The retained intent is honored by the next verdict.
	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})
	if got := taskStatus(t, s, task.ID); got != "killed" {
		t.Fatalf("status = %q, want killed (retained flag must settle the verdict)", got)
	}
}

// Manual retry clears a stale flag: a kill refused earlier must not
// assassinate an explicitly retried fresh attempt.
func TestClearKillRequestProtectsManualRetry(t *testing.T) {
	fl := &fakeRemoteLauncher{}
	s, q, _ := newKillRaceHarness(t, fl)
	task := seedRunningTask(t, s, q, "t-cleared")

	s.RequestKill(task.ID)
	s.ClearKillRequest(task.ID) // what the manual-retry path now does
	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})

	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("status = %q, want pending (cleared flag must not kill)", got)
	}
}
