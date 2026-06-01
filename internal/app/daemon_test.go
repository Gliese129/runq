package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/service"
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
		nil, // FIFO prioritizer
		"",  // socketPath: not exercised in this test
		nil, // freeze: this test predates L2-C, leave disabled
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

// TestL2CStage1EndToEnd exercises the post-pivot pipeline (artifacts + env
// injection + reap). The freeze path is exercised separately in
// api/server_test.go via the HTTP layer because freeze is SDK-driven now
// (not reap-driven from metrics.jsonl).
//
// What this test covers:
//  1. SubmitJob creates <root>/<job_id>/<task_id>/ with params.json
//     and wandb_config.json artifacts.
//  2. Scheduler dispatches the task with the L2-C env block injected, so
//     the command can find RUNQ_METRICS_FILE / RUNQ_TASK_DIR.
//  3. The task writes a metric and a checkpoint to metrics.jsonl using
//     the flat SDK event format (no `data` wrapper).
//  4. After the task exits, scheduler.runTask invokes shared ingest
//     which parses metrics.jsonl and batch-inserts metric + checkpoint
//     rows into the store. TaskID and JobID are filled by the daemon
//     from the task context (not by the SDK).
func TestL2CStage1EndToEnd(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()

	// ── construct daemon-equivalent harness (no Run loop, just components) ──
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := project.NewRegistry(st.DB())
	if err := reg.Add(project.Config{
		ProjectName: "p",
		WorkingDir:  workDir,
		// Write two flat SDK events into RUNQ_METRICS_FILE then exit.
		// Field names match the post-pivot contract: snake_case, no `data`
		// wrapper, no task_id/job_id (daemon fills those).
		CmdTemplate: `printf '%s\n' '{"type":"metric","key":"loss","value":0.4,"step":1,"ts":1700000000}' '{"type":"checkpoint","path":"/p","size_bytes":1024,"step":1,"is_best":true,"ts":1700000001}' > "$RUNQ_METRICS_FILE" # {{args}}`,
		Defaults:    project.Defaults{GPUsPerTask: 1},
		Wandb:       &project.WandbConfig{Project: "my-exp", Entity: "me"},
	}); err != nil {
		t.Fatalf("register project: %v", err)
	}

	queue := scheduler.NewQueue()
	pool := resource.NewMockAllocator(1)
	exec := executor.New()
	cfg := scheduler.DefaultConfig()
	cfg.TickInterval = 20 * time.Millisecond
	cfg.GPURefreshInterval = 0
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	freeze := scheduler.NewFreezeState()
	sched := scheduler.New(
		cfg, queue, pool, exec, st, logger, nil,
		"/tmp/runq-test.sock", freeze,
	)

	jobSvc := &service.JobService{
		Store: st, Queue: queue, Scheduler: sched, Exec: exec, Registry: reg, Pool: pool,
	}

	// ── submit a 1-task job ──
	jobID, n, err := jobSvc.SubmitJobWithOpts(
		ctx,
		job.JobConfig{
			Project: "p",
			Sweep: []job.SweepBlock{
				{
					Method: "grid",
					Parameters: map[string]job.ParameterSpec{
						"lr": {Values: []any{1e-4}},
					},
				},
			},
		},
		service.SubmitJobOpts{SkipPreflight: true},
	)
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 task, got %d", n)
	}

	// Verify Task 1.3 artifacts before dispatch.
	tasks, _ := st.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task row, got %d", len(tasks))
	}
	taskID := tasks[0].ID
	taskDir := filepath.Join(workDir, ".runq", jobID, taskID)
	if got := tasks[0].TaskDir; got != taskDir {
		t.Errorf("TaskRow.TaskDir = %q, want %q", got, taskDir)
	}
	for _, f := range []string{"params.json", "wandb_config.json"} {
		if _, err := os.Stat(filepath.Join(taskDir, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}

	// ── start scheduler, wait for completion ──
	sched.Start()
	defer sched.Shutdown()

	deadline := time.Now().Add(15 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		row, _ := st.GetTask(ctx, taskID)
		if row != nil && (row.Status == "success" || row.Status == "failed" || row.Status == "killed") {
			finalStatus = row.Status
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if finalStatus == "" {
		t.Fatal("task did not reach terminal status within 15s")
	}

	// ── verify reap landed data in metrics + checkpoints ──
	metrics, _ := st.ListMetrics(ctx, taskID, "")
	if len(metrics) != 1 {
		t.Errorf("expected 1 metric row, got %d (%+v)", len(metrics), metrics)
	} else if metrics[0].Key != "loss" {
		t.Errorf("metric key = %q, want %q", metrics[0].Key, "loss")
	}

	checkpoints, _ := st.ListCheckpoints(ctx, taskID)
	if len(checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint row, got %d (%+v)", len(checkpoints), checkpoints)
	}
	if !checkpoints[0].IsBest {
		t.Errorf("checkpoint is_best = false, want true")
	}
	if checkpoints[0].SizeBytes != 1024 {
		t.Errorf("checkpoint size_bytes = %d, want 1024", checkpoints[0].SizeBytes)
	}
	// Daemon-filled fields (NOT in the SDK payload).
	if checkpoints[0].TaskID != taskID || checkpoints[0].JobID != jobID {
		t.Errorf("checkpoint task/job not filled from context: %+v", checkpoints[0])
	}

	// ── freeze was NOT triggered (no disk_low in jsonl in the new model) ──
	if freeze.IsFrozen() {
		t.Error("FreezeState should remain empty — freeze is SDK-driven via HTTP, not reap")
	}
}
