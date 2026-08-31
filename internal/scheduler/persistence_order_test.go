package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

func TestMarkUnknownPersistenceFailureLeavesQueueRunning(t *testing.T) {
	s, q, _ := newKillRaceHarness(t, &fakeRemoteLauncher{})
	task := seedRunningTask(t, s, q, "t-unknown-persist-fail")
	if _, err := s.store.DB().Exec(`
		CREATE TRIGGER fail_unknown BEFORE UPDATE OF status ON tasks
		WHEN NEW.status = 'unknown'
		BEGIN SELECT RAISE(ABORT, 'injected unknown write failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	s.markUnknown(task, ErrLaunchUntracked)

	if got := q.Get(task.ID); got == nil || got.Status != StatusRunning {
		t.Fatalf("queue advanced despite failed persistence: %+v", got)
	}
	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("DB status = %q, want original pending", got)
	}
}

func TestAutomaticRetryPersistenceFailureLeavesQueueRunning(t *testing.T) {
	s, q, _ := newKillRaceHarness(t, &fakeRemoteLauncher{})
	task := seedRunningTask(t, s, q, "t-retry-persist-fail")
	s.slots.Reserve(task.ID)
	if _, err := s.store.DB().Exec(`
		CREATE TRIGGER fail_retry BEFORE UPDATE OF retry_count ON tasks
		WHEN NEW.retry_count > OLD.retry_count
		BEGIN SELECT RAISE(ABORT, 'injected retry write failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	s.FinishTask(task, StatusFailed, nil)

	if got := q.Get(task.ID); got == nil || got.Status != StatusRunning || got.RetryCount != 0 {
		t.Fatalf("queue requeued despite failed persistence: %+v", got)
	}
	row, err := s.store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.Status != "pending" || row.RetryCount != 0 {
		t.Fatalf("durable retry changed despite rollback: status=%q retry=%d", row.Status, row.RetryCount)
	}
	if s.slots.FreeCount() != 0 {
		t.Fatal("submission slot released despite failed retry persistence")
	}
}

func TestAutomaticRetryCarriesCompleteAttemptFence(t *testing.T) {
	s, q, _ := newKillRaceHarness(t, &fakeRemoteLauncher{})
	task := seedRunningTask(t, s, q, "t-retry-fence")
	if err := s.store.UpdateTaskStatus(context.Background(), task.ID, "running", map[string]any{
		"status_source":     "scheduler",
		"external_id":       "attempt-0",
		"target_generation": "generation-a",
		"native_state":      "RUNNING",
		"queue":             "gpu",
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.store.GetTask(context.Background(), task.ID)
	if err != nil || loaded == nil {
		t.Fatalf("load attempt: %v", err)
	}
	extra := map[string]any{"status_source": "wrapper"}
	store.FenceTaskStatusUpdate(extra, *loaded)

	// A stronger concurrent verdict wins durably while this failure remains
	// queued in the stale scheduler view. The failed→pending reducer must keep
	// the full fence and therefore must not reopen the terminal row.
	if err := s.store.UpdateTaskStatus(context.Background(), task.ID, "success", map[string]any{
		"status_source": "wrapper",
	}); err != nil {
		t.Fatal(err)
	}
	s.FinishTask(task, StatusFailed, extra)

	row, err := s.store.GetTask(context.Background(), task.ID)
	if err != nil || row == nil {
		t.Fatalf("read winner: %v", err)
	}
	if row.Status != "success" || row.RetryCount != 0 {
		t.Fatalf("stale automatic retry reopened winner: %#v", row)
	}
	if got := q.Get(task.ID); got == nil || got.Status != StatusRunning {
		t.Fatalf("queue advanced after rejected stale transition: %+v", got)
	}
}

func TestAutomaticRetryClearsPriorAttemptEvidence(t *testing.T) {
	s, q, _ := newKillRaceHarness(t, &fakeRemoteLauncher{})
	task := seedRunningTask(t, s, q, "t-retry-clean")
	if err := s.store.UpdateTaskStatus(context.Background(), task.ID, "running", map[string]any{
		"status_source":     "scheduler",
		"external_id":       "attempt-0",
		"target_generation": "generation-a",
		"native_state":      "RUNNING",
		"queue":             "gpu",
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.store.GetTask(context.Background(), task.ID)
	if err != nil || loaded == nil {
		t.Fatalf("load attempt: %v", err)
	}
	extra := map[string]any{"status_source": "wrapper"}
	store.FenceTaskStatusUpdate(extra, *loaded)
	s.FinishTask(task, StatusFailed, extra)

	row, err := s.store.GetTask(context.Background(), task.ID)
	if err != nil || row == nil {
		t.Fatalf("read retry: %v", err)
	}
	if row.Status != "pending" || row.RetryCount != 1 || row.StatusSource != "" ||
		row.ExternalID != "" || row.NativeState != "" || row.Queue != "" {
		t.Fatalf("fresh retry retained prior-attempt evidence: %#v", row)
	}
	if got := q.Get(task.ID); got == nil || got.Status != StatusPending || got.RetryCount != 1 {
		t.Fatalf("queue retry = %+v", got)
	}
}

type observingRemoteLauncher struct {
	onLaunch func()
	release  chan struct{}
}

func (l *observingRemoteLauncher) Launch(_ context.Context, _ *Task) error {
	if l.onLaunch != nil {
		l.onLaunch()
	}
	if l.release != nil {
		<-l.release
	}
	return ErrLaunchTransient
}

func (l *observingRemoteLauncher) Kill(string) error { return nil }

func TestRemoteDispatchPersistsSubmittingBeforeExternalEffect(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	pool := testPool(1)
	task := &Task{
		ID: "t-submit-intent", JobID: "j-submit-intent", ProjectName: "test",
		Command: "submit", GPUsNeeded: 1,
	}
	seedJob(t, st, task.JobID, task.ProjectName, 1)
	seedTask(t, st, task)
	q.Push(task)
	observed := make(chan string, 1)
	release := make(chan struct{})
	launcher := &observingRemoteLauncher{
		release: release,
		onLaunch: func() {
			row, err := st.GetTask(context.Background(), task.ID)
			if err != nil {
				observed <- "error: " + err.Error()
				return
			}
			observed <- row.Status
		},
	}
	s := New(DefaultConfig(), q, pool, launcher, st, testLogger())

	s.dispatch(task)
	select {
	case got := <-observed:
		if got != "submitting" {
			t.Fatalf("status at external-effect boundary = %q, want submitting", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launcher did not observe submitting intent")
	}
	if got := q.Get(task.ID); got == nil || got.Status != StatusRunning {
		t.Fatalf("queue did not retain in-flight ownership: %+v", got)
	}
	if pool.FreeCount() != 0 {
		t.Fatal("submission slot was not retained while launch was in flight")
	}
	close(release)
	s.wg.Wait()
	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("transient pre-start error did not durably return to pending: %q", got)
	}
	if got := jobStatus(t, st, task.JobID); got != "pending" {
		t.Fatalf("job status after transient pre-start error = %q, want pending", got)
	}
}

func TestPauseAcknowledgementIsLinearizableWithExternalHandoff(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	pool := testPool(1)
	task := &Task{
		ID: "t-pause-linearizable", JobID: "j-pause-linearizable", ProjectName: "test",
		Command: "submit", GPUsNeeded: 1,
	}
	seedJob(t, st, task.JobID, task.ProjectName, 1)
	seedTask(t, st, task)
	q.Push(task)

	entered := make(chan struct{})
	release := make(chan struct{})
	var launches atomic.Int32
	launcher := &observingRemoteLauncher{
		release: release,
		onLaunch: func() {
			if launches.Add(1) == 1 {
				close(entered)
			}
		},
	}
	s := New(DefaultConfig(), q, pool, launcher, st, testLogger())
	s.dispatch(task)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("external handoff did not start")
	}

	pauseStarted := make(chan struct{})
	pauseDone := make(chan struct{})
	go func() {
		close(pauseStarted)
		s.PauseJob(task.JobID)
		close(pauseDone)
	}()
	<-pauseStarted
	select {
	case <-pauseDone:
		t.Fatal("pause acknowledged while an admitted external handoff was still blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-pauseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not acknowledge after external handoff returned")
	}
	s.wg.Wait()

	// The transient handoff returned the task to pending. Once pause has
	// acknowledged, another tick must not begin a second external handoff.
	s.tick()
	s.wg.Wait()
	if got := launches.Load(); got != 1 {
		t.Fatalf("external handoffs after acknowledged pause = %d, want 1 total", got)
	}
}

func TestSubmittingPersistenceFailurePreventsExternalLaunch(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	pool := testPool(1)
	task := &Task{
		ID: "t-submit-persist-fail", JobID: "j-submit-persist-fail", ProjectName: "test",
		Command: "submit", GPUsNeeded: 1,
	}
	seedJob(t, st, task.JobID, task.ProjectName, 1)
	seedTask(t, st, task)
	q.Push(task)
	if _, err := st.DB().Exec(`
		CREATE TRIGGER fail_submitting BEFORE UPDATE OF status ON tasks
		WHEN NEW.status = 'submitting'
		BEGIN SELECT RAISE(ABORT, 'injected submitting write failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	launched := make(chan struct{}, 1)
	launcher := &observingRemoteLauncher{onLaunch: func() { launched <- struct{}{} }}
	s := New(DefaultConfig(), q, pool, launcher, st, testLogger())

	s.dispatch(task)

	select {
	case <-launched:
		t.Fatal("external launch occurred without durable submitting intent")
	default:
	}
	if got := q.Get(task.ID); got == nil || got.Status != StatusPending {
		t.Fatalf("queue advanced despite failed submitting persistence: %+v", got)
	}
	if pool.FreeCount() != 1 {
		t.Fatal("submission slot was not released after pre-effect persistence failure")
	}
}

func TestTransientRequeuePersistenceFailureRetainsSubmittingOwnership(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	pool := testPool(1)
	task := &Task{
		ID: "t-transient-persist-fail", JobID: "j-transient-persist-fail", ProjectName: "test",
		Command: "submit", GPUsNeeded: 1,
	}
	seedJob(t, st, task.JobID, task.ProjectName, 1)
	seedTask(t, st, task)
	q.Push(task)
	if err := st.UpdateTaskStatus(context.Background(), task.ID, "submitting", nil); err != nil {
		t.Fatalf("mark submitting: %v", err)
	}
	if err := q.MarkRunning(task.ID); err != nil {
		t.Fatalf("mark queue running: %v", err)
	}
	pool.Reserve(task.ID)
	if _, err := st.DB().Exec(`
		CREATE TRIGGER fail_transient_pending BEFORE UPDATE OF status ON tasks
		WHEN OLD.status = 'submitting' AND NEW.status = 'pending'
		BEGIN SELECT RAISE(ABORT, 'injected transient requeue failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	s := New(DefaultConfig(), q, pool, &observingRemoteLauncher{}, st, testLogger())

	if s.requeueTransient(task) {
		t.Fatal("transient requeue reported success after persistence failure")
	}
	if got := taskStatus(t, s, task.ID); got != "submitting" {
		t.Fatalf("durable status = %q, want submitting", got)
	}
	if got := q.Get(task.ID); got == nil || got.Status != StatusRunning {
		t.Fatalf("queue ownership was released despite failed persistence: %+v", got)
	}
	if pool.FreeCount() != 0 {
		t.Fatal("submission slot was released despite failed persistence")
	}
}

func TestTerminalPersistenceFailureRetainsSubmissionSlot(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	pool := testPool(1)
	task := &Task{
		ID: "t-terminal-persist-fail", JobID: "j-terminal-persist-fail", ProjectName: "test",
		Command: "submit", GPUsNeeded: 1, MaxRetry: 0, Status: StatusRunning,
	}
	seedJob(t, st, task.JobID, task.ProjectName, 1)
	seedTask(t, st, task)
	q.Push(task)
	task.Status = StatusRunning
	pool.Reserve(task.ID)
	if _, err := st.DB().Exec(`
		CREATE TRIGGER fail_terminal BEFORE UPDATE OF status ON tasks
		WHEN NEW.status IN ('failed', 'killed', 'success')
		BEGIN SELECT RAISE(ABORT, 'injected terminal write failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	s := New(DefaultConfig(), q, pool, &observingRemoteLauncher{}, st, testLogger())
	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})

	if got := q.Get(task.ID); got == nil || got.Status != StatusRunning {
		t.Fatalf("queue advanced despite failed terminal persistence: %+v", got)
	}
	if got := taskStatus(t, s, task.ID); got != "pending" {
		t.Fatalf("DB status = %q, want original pending", got)
	}
	if pool.FreeCount() != 0 {
		t.Fatal("submission slot released before the terminal transition became durable")
	}
}
