package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/resource"
	"github.com/gliese129/runq-lab/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testPool(n int) *resource.SlotAllocator {
	return resource.NewSlotAllocator(n)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedProject(t *testing.T, s *store.Store, name string) {
	t.Helper()
	if _, err := s.DB().Exec(
		`INSERT OR IGNORE INTO projects (name, config_json) VALUES (?, '{}')`, name,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func seedJob(t *testing.T, s *store.Store, jobID, project string, totalTasks int) {
	t.Helper()
	seedProject(t, s, project)
	if err := s.InsertJob(context.Background(), &store.JobRow{
		ID: jobID, ProjectName: project, ConfigJSON: "{}",
		Status: "pending", TotalTasks: totalTasks, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

func seedTask(t *testing.T, s *store.Store, task *Task) {
	t.Helper()
	if err := s.InsertTask(context.Background(), &store.TaskRow{
		ID: task.ID, JobID: task.JobID, ProjectName: task.ProjectName,
		Command: task.Command, ParamsJSON: "{}", GPUsNeeded: task.GPUsNeeded,
		Status: "pending", MaxRetry: task.MaxRetry, LogPath: task.LogPath,
		WorkingDir: task.WorkingDir, EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func TestSchedulerUsesOneSlotPerRemoteTask(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	launcher := &fakeRemoteLauncher{}
	s := New(DefaultConfig(), q, testPool(1), launcher, st, testLogger())

	first := &Task{ID: "t1", JobID: "j1", ProjectName: "test", GPUsNeeded: 8}
	second := &Task{ID: "t2", JobID: "j1", ProjectName: "test", GPUsNeeded: 1}
	seedJob(t, st, "j1", "test", 2)
	seedTask(t, st, first)
	seedTask(t, st, second)
	q.Push(first)
	q.Push(second)

	s.tick()
	s.wg.Wait()
	s.tick()

	if got := launcher.launchedIDs(); len(got) != 1 || got[0] != first.ID {
		t.Fatalf("launches with one occupied slot = %v, want [%s]", got, first.ID)
	}
	if second.Status != StatusPending {
		t.Fatalf("second task status = %s, want pending while max_inflight is full", second.Status)
	}

	s.FinishTask(first, StatusSuccess, map[string]any{"status_source": "wrapper"})
	s.tick()
	s.wg.Wait()
	if got := launcher.launchedIDs(); len(got) != 2 || got[1] != second.ID {
		t.Fatalf("launches after slot release = %v, want second task dispatched", got)
	}
}

func TestSchedulerSkipsPausedJobs(t *testing.T) {
	q := NewQueue()
	st := testStore(t)
	launcher := &fakeRemoteLauncher{}
	s := New(DefaultConfig(), q, testPool(1), launcher, st, testLogger())

	paused := &Task{ID: "t-paused", JobID: "j-paused", ProjectName: "test", GPUsNeeded: 1}
	ready := &Task{ID: "t-ready", JobID: "j-ready", ProjectName: "test", GPUsNeeded: 1}
	seedJob(t, st, paused.JobID, "test", 1)
	seedTask(t, st, paused)
	seedJob(t, st, ready.JobID, "test", 1)
	seedTask(t, st, ready)
	q.Push(paused)
	q.Push(ready)
	s.PauseJob(paused.JobID)

	s.tick()
	s.wg.Wait()
	if got := launcher.launchedIDs(); len(got) != 1 || got[0] != ready.ID {
		t.Fatalf("launches = %v, want only unpaused task %s", got, ready.ID)
	}
}

func TestRemoteFailureRetriesThenFailsPermanently(t *testing.T) {
	launcher := &fakeRemoteLauncher{}
	s, q, _ := newKillRaceHarness(t, launcher)
	task := seedRunningTask(t, s, q, "t-retry")
	task.MaxRetry = 1

	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})
	if got := q.Get(task.ID); got == nil || got.Status != StatusPending || got.RetryCount != 1 {
		t.Fatalf("first failure did not requeue: %+v", got)
	}
	if s.slots.FreeCount() != 1 {
		t.Fatal("slot was not released after durable retry transition")
	}

	s.dispatch(task)
	s.wg.Wait()
	s.FinishTask(task, StatusFailed, map[string]any{"status_source": "probe"})
	if got := taskStatus(t, s, task.ID); got != "failed" {
		t.Fatalf("status after exhausted retry = %q, want failed", got)
	}
	if s.slots.FreeCount() != 1 {
		t.Fatal("slot was not released after durable terminal transition")
	}
}

func TestHandleFailureLogsNextRetryOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	q := NewQueue()
	st := testStore(t)
	task := &Task{
		ID: "t-fail", JobID: "j1", ProjectName: "test",
		Command: "exit 1", GPUsNeeded: 1, MaxRetry: 1,
	}
	seedJob(t, st, "j1", "test", 1)
	seedTask(t, st, task)
	q.Push(task)

	s := New(DefaultConfig(), q, testPool(1), &fakeRemoteLauncher{}, st, logger)
	s.handleFailure(task, nil)

	logs := buf.String()
	if !strings.Contains(logs, "retry=1") {
		t.Fatalf("expected retry=1 in logs, got %q", logs)
	}
	if strings.Contains(logs, "retry=2") {
		t.Fatalf("unexpected retry=2 in logs: %q", logs)
	}

	row, err := st.GetTask(context.Background(), "t-fail")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.RetryCount != 1 || q.Get(task.ID).RetryCount != 1 {
		t.Fatalf("retry count mismatch: db=%d queue=%d", row.RetryCount, q.Get(task.ID).RetryCount)
	}
}
