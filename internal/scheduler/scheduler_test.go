package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/gpu"
	"github.com/gliese129/runq/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testPool(n int) *GPUPool {
	infos := make([]gpu.Info, n)
	for i := range infos {
		infos[i] = gpu.Info{Index: i, MemFree: 80000}
	}
	return NewGPUPool(infos)
}

// testStore opens an in-memory SQLite store for testing.
// The scheduler persists state on every dispatch/complete, so tests need a real store.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedProject inserts a minimal project row so that job foreign keys are satisfied.
func seedProject(t *testing.T, s *store.Store, name string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT OR IGNORE INTO projects (name, config_json) VALUES (?, '{}')`, name,
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// seedJob inserts a minimal job row so that task foreign keys are satisfied.
func seedJob(t *testing.T, s *store.Store, jobID, project string, totalTasks int) {
	t.Helper()
	seedProject(t, s, project)
	err := s.InsertJob(context.Background(), &store.JobRow{
		ID: jobID, ProjectName: project, ConfigJSON: "{}",
		Status: "pending", TotalTasks: totalTasks, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

// seedTask inserts a minimal task row so the scheduler can update it.
func seedTask(t *testing.T, s *store.Store, task *Task) {
	t.Helper()
	err := s.InsertTask(context.Background(), &store.TaskRow{
		ID: task.ID, JobID: task.JobID, ProjectName: task.ProjectName,
		Command: task.Command, ParamsJSON: "{}", GPUsNeeded: task.GPUsNeeded,
		Status: "pending", MaxRetry: task.MaxRetry, LogPath: task.LogPath,
		WorkingDir: task.WorkingDir, EnqueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func TestSchedulerDispatchSingle(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	task := &Task{
		ID: "t1", JobID: "j1", GPUsNeeded: 1,
		Command: `echo "dispatched"`, WorkingDir: dir,
		LogPath: filepath.Join(dir, "t1.log"),
	}
	seedJob(t, st, "j1", "test", 1)
	seedTask(t, st, task)

	s := New(cfg, q, pool, exec, st, logger)
	s.Start()
	q.Push(task)

	time.Sleep(1 * time.Second)
	s.Shutdown()

	got := q.Get("t1")
	if got.Status != StatusSuccess {
		t.Errorf("expected success, got %s", got.Status)
	}
	if pool.FreeCount() != 4 {
		t.Errorf("expected 4 free GPUs, got %d", pool.FreeCount())
	}
}

func TestSchedulerRetry(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(2)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	task := &Task{
		ID: "t-fail", JobID: "j1", GPUsNeeded: 1,
		Command: "exit 1", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-fail.log"), MaxRetry: 2,
	}
	seedJob(t, st, "j1", "test", 1)
	seedTask(t, st, task)

	s := New(cfg, q, pool, exec, st, logger)
	s.Start()
	q.Push(task)

	time.Sleep(2 * time.Second)
	s.Shutdown()

	got := q.Get("t-fail")
	if got.Status != StatusFailed {
		t.Errorf("expected failed, got %s", got.Status)
	}
	if got.RetryCount != 2 {
		t.Errorf("expected 2 retries, got %d", got.RetryCount)
	}
}

func TestSchedulerBackfill(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.BackfillEnabled = true
	cfg.AgingThreshold = 1 * time.Hour

	big := &Task{
		ID: "t-big", JobID: "j1", GPUsNeeded: 8,
		Command: "echo big", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-big.log"),
	}
	small := &Task{
		ID: "t-small", JobID: "j1", GPUsNeeded: 1,
		Command: "echo small", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-small.log"),
	}
	seedJob(t, st, "j1", "test", 2)
	seedTask(t, st, big)
	seedTask(t, st, small)

	s := New(cfg, q, pool, exec, st, logger)
	s.Start()
	q.Push(big)
	q.Push(small)

	time.Sleep(1 * time.Second)
	s.Shutdown()

	gotSmall := q.Get("t-small")
	if gotSmall.Status != StatusSuccess {
		t.Errorf("expected small task success, got %s", gotSmall.Status)
	}
	gotBig := q.Get("t-big")
	if gotBig.Status != StatusPending {
		t.Errorf("expected big task pending, got %s", gotBig.Status)
	}
}

func TestSchedulerReservation(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.BackfillEnabled = true
	cfg.AgingThreshold = 1 * time.Millisecond

	big := &Task{
		ID: "t-big", JobID: "j1", GPUsNeeded: 4,
		Command: "echo big", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-big.log"),
	}
	small := &Task{
		ID: "t-small", JobID: "j1", GPUsNeeded: 1,
		Command: "echo small", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-small.log"),
	}
	seedJob(t, st, "j1", "test", 2)
	seedTask(t, st, big)
	seedTask(t, st, small)

	// Occupy 2 GPUs so big task can't fit.
	pool.Allocate(2, "external-task")

	s := New(cfg, q, pool, exec, st, logger)
	s.Start()
	q.Push(big)
	q.Push(small)

	// Reservation mode should block backfill.
	time.Sleep(500 * time.Millisecond)
	gotSmall := q.Get("t-small")
	if gotSmall.Status != StatusPending {
		t.Errorf("expected small task blocked by reservation, got %s", gotSmall.Status)
	}

	// Free external GPUs → big task gets scheduled.
	pool.Release("external-task")
	time.Sleep(1 * time.Second)
	s.Shutdown()

	gotBig := q.Get("t-big")
	if gotBig.Status != StatusSuccess {
		t.Errorf("expected big task success, got %s", gotBig.Status)
	}
}
