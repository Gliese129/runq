package backend

import (
	"context"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

func TestPerformCleanExactSelectionKeepsUnknownTask(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-1", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 2, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	for _, row := range []store.TaskRow{
		{
			ID: "unknown-task", JobID: "job-1", ProjectName: "p",
			Command: "true", ParamsJSON: "{}", GPUsNeeded: 1,
			Status: "unknown", EnqueuedAt: time.Now(),
		},
		{
			ID: "failed-task", JobID: "job-1", ProjectName: "p",
			Command: "true", ParamsJSON: "{}", GPUsNeeded: 1,
			Status: "failed", EnqueuedAt: time.Now(),
		},
	} {
		if err := st.InsertTask(ctx, &row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}

	result, err := PerformClean(ctx, st, nil, CleanOptions{
		TaskIDs: []string{"unknown-task", "failed-task"},
	})
	if err != nil {
		t.Fatalf("exact clean: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("deleted tasks = %d, want only the terminal task", result.Tasks)
	}
	unknown, err := st.GetTask(ctx, "unknown-task")
	if err != nil {
		t.Fatalf("get unknown task: %v", err)
	}
	if unknown == nil || unknown.Status != "unknown" {
		t.Fatalf("unknown task was removed or changed: %+v", unknown)
	}
	failed, err := st.GetTask(ctx, "failed-task")
	if err != nil {
		t.Fatalf("get failed task: %v", err)
	}
	if failed != nil {
		t.Fatalf("terminal task was not cleaned: %+v", failed)
	}
}
