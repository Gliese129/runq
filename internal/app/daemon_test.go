package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

func TestRestoreRuntimeStateRestoresPausedJobsBeforeScheduling(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('test', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "test", ConfigJSON: "{}",
		Status: "paused", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	dir := t.TempDir()
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "test",
		Command: "echo should-not-run", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "pending", WorkingDir: dir, LogPath: filepath.Join(dir, "logs", "t1.log"),
		EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	queue := scheduler.NewQueue()
	exec := executor.New()
	cfg := scheduler.DefaultConfig()
	cfg.TickInterval = 20 * time.Millisecond
	sched := scheduler.New(
		cfg,
		queue,
		resource.NewMockAllocator(1),
		exec,
		st,
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)
	d := &Daemon{
		Store:     st,
		Scheduler: sched,
		Logger:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Executor:  exec,
		Queue:     queue,
	}

	if err := d.restoreRuntimeState(); err != nil {
		t.Fatalf("restoreRuntimeState: %v", err)
	}
	sched.Start()
	defer sched.Shutdown()

	time.Sleep(100 * time.Millisecond)

	task := queue.Get("t1")
	if task == nil {
		t.Fatal("expected task restored into queue")
	}
	if task.Status != scheduler.StatusPending {
		t.Fatalf("paused job task should remain pending, got %s", task.Status)
	}
	row, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.Status != "pending" {
		t.Fatalf("DB task should remain pending, got %s", row.Status)
	}
}
