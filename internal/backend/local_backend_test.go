package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
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
	be := NewLocalBackend(LocalBackendDeps{
		Store: st,
		Queue: q,
		Exec:  executor.New(),
	})

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

	killed, err := be.KillJobRaw(ctx, "j1")
	if err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	if killed != 2 {
		t.Fatalf("expected 2 killed tasks, got %d", killed)
	}

	j, err := st.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.Status != "done" {
		t.Fatalf("expected job status done, got %q", j.Status)
	}
	if j.FinishedAt == nil {
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

// setupSubmitTest spins up the minimum dependencies for submitJob: store,
// registry with one project, queue, mock allocator.
func setupSubmitTest(t *testing.T, workDir string, wandb *project.WandbConfig) (*LocalBackend, job.JobConfig) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := project.NewRegistry(st.DB())
	if err := reg.Add(ctx, project.Config{
		ProjectName: "p",
		WorkingDir:  workDir,
		CmdTemplate: "echo {{args}}",
		Defaults:    project.Defaults{GPUsPerTask: 1},
		Wandb:       wandb,
	}); err != nil {
		t.Fatalf("registry add: %v", err)
	}

	q := scheduler.NewQueue()
	be := NewLocalBackend(LocalBackendDeps{
		Store: st,
		Reg:   reg,
		Queue: q,
		Exec:  executor.New(),
	})
	_ = ctx

	jobCfg := job.JobConfig{
		Project: "p",
		Sweep: []job.SweepBlock{
			{
				Method: "grid",
				Parameters: map[string]job.ParameterSpec{
					"lr": {Values: []any{1e-4, 1e-3}},
				},
			},
		},
	}
	return be, jobCfg
}

func TestSubmitJobCreatesTaskDir(t *testing.T) {
	workDir := t.TempDir()
	wandb := &project.WandbConfig{
		Project: "my-exp",
		Entity:  "me",
		Tags:    []string{"baseline"},
	}
	be, jobCfg := setupSubmitTest(t, workDir, wandb)

	jobID, n, err := be.SubmitJobRaw(context.Background(), jobCfg, true)
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 tasks expanded, got %d", n)
	}

	tasks, err := be.store.ListTasks(context.Background(), store.TaskFilter{JobID: jobID})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks persisted, got %d", len(tasks))
	}

	for _, tr := range tasks {
		taskDir := filepath.Join(workDir, ".runq", jobID, tr.ID)
		if tr.TaskDir != taskDir {
			t.Errorf("TaskRow.TaskDir = %q, want %q", tr.TaskDir, taskDir)
		}
		assertDirExists(t, taskDir)
		assertDirExists(t, filepath.Join(taskDir, "checkpoints"))

		paramsBytes, err := os.ReadFile(filepath.Join(taskDir, "params.json"))
		if err != nil {
			t.Fatalf("read params.json: %v", err)
		}
		var params map[string]any
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			t.Fatalf("unmarshal params.json: %v", err)
		}
		if _, ok := params["lr"]; !ok {
			t.Errorf("params.json missing 'lr', got %v", params)
		}

		wcBytes, err := os.ReadFile(filepath.Join(taskDir, "wandb_config.json"))
		if err != nil {
			t.Fatalf("read wandb_config.json: %v", err)
		}
		var wc project.WandbConfig
		if err := json.Unmarshal(wcBytes, &wc); err != nil {
			t.Fatalf("unmarshal wandb_config.json: %v", err)
		}
		if wc.Project != "my-exp" {
			t.Errorf("wandb_config.json project = %q, want %q", wc.Project, "my-exp")
		}
	}
}

func TestSubmitJobPersistsConfiguredLocalTarget(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	be, jobCfg := setupSubmitTest(t, workDir, nil)
	be.targetName = "workstation"

	jobID, _, err := be.SubmitJob(ctx, jobCfg, SubmitOptions{SkipPreflight: true})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}

	j, err := be.store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if j.Target != "workstation" {
		t.Fatalf("JobRow.Target = %q, want workstation", j.Target)
	}
	tasks, err := be.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	for _, task := range tasks {
		if task.Target != "workstation" {
			t.Fatalf("TaskRow.Target for %s = %q, want workstation", task.ID, task.Target)
		}
	}

	multi, err := NewMultiBackend(map[string]Backend{"workstation": be}, be.store, "workstation")
	if err != nil {
		t.Fatalf("NewMultiBackend: %v", err)
	}
	if _, err := multi.GetJob(ctx, jobID); err != nil {
		t.Fatalf("GetJob via MultiBackend: %v", err)
	}
	if err := multi.KillJob(ctx, jobID); err != nil {
		t.Fatalf("KillJob via MultiBackend: %v", err)
	}
}

func TestSubmitJobNoWandb(t *testing.T) {
	workDir := t.TempDir()
	be, jobCfg := setupSubmitTest(t, workDir, nil)

	jobID, _, err := be.SubmitJobRaw(context.Background(), jobCfg, true)
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	tasks, _ := be.store.ListTasks(context.Background(), store.TaskFilter{JobID: jobID})
	for _, tr := range tasks {
		path := filepath.Join(workDir, ".runq", jobID, tr.ID, "wandb_config.json")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("no wandb block → wandb_config.json should not exist (path=%s, err=%v)", path, err)
		}
	}
}

func TestSubmitJobMissingWorkingDir(t *testing.T) {
	workDir := t.TempDir()
	be, jobCfg := setupSubmitTest(t, workDir, nil)
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatalf("remove working_dir: %v", err)
	}
	_, _, err := be.SubmitJobRaw(context.Background(), jobCfg, true)
	if err == nil {
		t.Fatal("expected error for missing working_dir, got nil")
	}
	if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("error should mention working_dir, got: %v", err)
	}
}

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

	be := NewLocalBackend(LocalBackendDeps{
		Store: st,
		Queue: q,
		Exec:  executor.New(),
	})
	if err := be.RetryTask(ctx, "t1"); err != nil {
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

func assertDirExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected dir %q to exist: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
}
