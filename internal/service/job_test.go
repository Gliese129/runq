package service

import (
	"context"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

func TestKillJobRefreshesAggregateStatusForPendingTasks(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	q := scheduler.NewQueue()
	svc := &JobService{
		Store: st,
		Queue: q,
		Exec:  executor.New(),
	}

	now := time.Now()
	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('test', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "test", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 2, CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	tasks := []*scheduler.Task{
		{ID: "t1", JobID: "j1", ProjectName: "test", Command: "sleep 1", GPUsNeeded: 1},
		{ID: "t2", JobID: "j1", ProjectName: "test", Command: "sleep 1", GPUsNeeded: 1},
	}
	for _, task := range tasks {
		if err := st.InsertTask(ctx, &store.TaskRow{
			ID: task.ID, JobID: task.JobID, ProjectName: task.ProjectName,
			Command: task.Command, ParamsJSON: "{}", GPUsNeeded: task.GPUsNeeded,
			Status: "pending", EnqueuedAt: now,
		}); err != nil {
			t.Fatalf("insert task %s: %v", task.ID, err)
		}
	}
	q.PushBatch(tasks)

	killed, err := svc.KillJob(ctx, "j1")
	if err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	if killed != 2 {
		t.Fatalf("expected 2 killed tasks, got %d", killed)
	}

	job, err := st.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != "done" {
		t.Fatalf("expected job status done, got %q", job.Status)
	}
	if job.FinishedAt == nil {
		t.Fatalf("expected finished_at to be set")
	}

	rows, err := st.ListTasks(ctx, store.TaskFilter{JobID: "j1"})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	for _, row := range rows {
		if row.Status != "killed" {
			t.Fatalf("expected task %s to be killed, got %q", row.ID, row.Status)
		}
	}
}
