package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/gpu"
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

func TestSchedulerDispatchSingle(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	s := New(cfg, q, pool, exec, logger)
	s.Start()

	q.Push(&Task{
		ID:         "t1",
		JobID:      "j1",
		GPUsNeeded: 1,
		Command:    `echo "dispatched"`,
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "t1.log"),
	})

	// Wait for task to complete
	time.Sleep(1 * time.Second)
	s.Shutdown()

	task := q.Get("t1")
	if task.Status != StatusSuccess {
		t.Errorf("expected success, got %s", task.Status)
	}
	if pool.FreeCount() != 4 {
		t.Errorf("expected 4 free GPUs after completion, got %d", pool.FreeCount())
	}
}

func TestSchedulerRetry(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(2)
	exec := executor.New()
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	s := New(cfg, q, pool, exec, logger)
	s.Start()

	q.Push(&Task{
		ID:         "t-fail",
		JobID:      "j1",
		GPUsNeeded: 1,
		Command:    "exit 1",
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "t-fail.log"),
		MaxRetry:   2,
	})

	// Wait enough time for retries
	time.Sleep(2 * time.Second)
	s.Shutdown()

	task := q.Get("t-fail")
	// Should have retried twice then failed (total attempts: 1 original + 2 retries)
	if task.Status != StatusFailed {
		t.Errorf("expected failed, got %s", task.Status)
	}
	if task.RetryCount != 2 {
		t.Errorf("expected 2 retries, got %d", task.RetryCount)
	}
}

func TestSchedulerBackfill(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.BackfillEnabled = true
	// Set aging very high so we stay in backfill mode
	cfg.AgingThreshold = 1 * time.Hour

	s := New(cfg, q, pool, exec, logger)
	s.Start()

	// Big task needs 8 GPUs (can't fit in 4-GPU pool)
	q.Push(&Task{
		ID:         "t-big",
		JobID:      "j1",
		GPUsNeeded: 8,
		Command:    "echo big",
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "t-big.log"),
	})
	// Small task that can fit
	q.Push(&Task{
		ID:         "t-small",
		JobID:      "j1",
		GPUsNeeded: 1,
		Command:    "echo small",
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "t-small.log"),
	})

	time.Sleep(1 * time.Second)
	s.Shutdown()

	// Small task should have been backfilled and completed
	small := q.Get("t-small")
	if small.Status != StatusSuccess {
		t.Errorf("expected small task success, got %s", small.Status)
	}

	// Big task should still be pending (not enough GPUs)
	big := q.Get("t-big")
	if big.Status != StatusPending {
		t.Errorf("expected big task pending, got %s", big.Status)
	}
}

func TestSchedulerReservation(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.BackfillEnabled = true
	// Very short aging so reservation kicks in immediately
	cfg.AgingThreshold = 1 * time.Millisecond

	s := New(cfg, q, pool, exec, logger)

	// Big task at head — needs all 4 GPUs. Allocate 2 so only 2 are free.
	pool.Allocate(2, "external-task")

	s.Start()

	q.Push(&Task{
		ID:         "t-big",
		JobID:      "j1",
		GPUsNeeded: 4,
		Command:    "echo big",
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "t-big.log"),
	})
	// Small task behind it
	q.Push(&Task{
		ID:         "t-small",
		JobID:      "j1",
		GPUsNeeded: 1,
		Command:    "echo small",
		WorkingDir: dir,
		LogPath:    filepath.Join(dir, "t-small.log"),
	})

	// Wait a bit — reservation mode should block backfill
	time.Sleep(500 * time.Millisecond)

	small := q.Get("t-small")
	if small.Status != StatusPending {
		t.Errorf("expected small task blocked by reservation, got %s", small.Status)
	}

	// Now free the external GPUs — big task should get scheduled
	pool.Release("external-task")
	time.Sleep(1 * time.Second)
	s.Shutdown()

	big := q.Get("t-big")
	if big.Status != StatusSuccess {
		t.Errorf("expected big task success after reservation, got %s", big.Status)
	}
}
