package service

import (
	"context"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

func TestRetryTaskUpdatesExistingQueueEntry(t *testing.T) {
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
		Status: "done", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "test",
		Command: "echo ok", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "failed", RetryCount: 1, MaxRetry: 3,
		EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	q := scheduler.NewQueue()
	q.Push(&scheduler.Task{
		ID: "t1", JobID: "j1", ProjectName: "test",
		Command: "echo ok", GPUsNeeded: 1,
		Status: scheduler.StatusFailed, RetryCount: 1, MaxRetry: 3,
	})
	if err := q.MarkRunning("t1"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := q.Complete("t1", scheduler.StatusFailed); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	svc := &TaskService{Store: st, Queue: q, Exec: executor.New()}
	if err := svc.RetryTask(ctx, "t1"); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected queue length 1, got %d", q.Len())
	}
	if got := q.Get("t1"); got.Status != scheduler.StatusPending {
		t.Fatalf("expected retried task pending, got %s", got.Status)
	}
	if err := q.MarkRunning("t1"); err != nil {
		t.Fatalf("retried task should be markable running: %v", err)
	}
}
