package submitplan

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

func TestBuildDeterministicPlan(t *testing.T) {
	workDir := t.TempDir()
	ids := []string{"job1", "task1", "task2"}
	nextID := func() string {
		if len(ids) == 0 {
			t.Fatalf("IDGen called too many times")
		}
		id := ids[0]
		ids = ids[1:]
		return id
	}
	gpus := 2
	maxRetry := 3
	timeout := "2h"
	cfg := job.JobConfig{
		Project:     "proj",
		Description: "demo",
		Sweep: []job.SweepBlock{
			{
				Method: "grid",
				Parameters: map[string]job.ParameterSpec{
					"lr": {Values: []any{0.1, 0.2}},
				},
			},
		},
		Overrides: &job.Overrides{
			GPUsPerTask: &gpus,
			MaxRetry:    &maxRetry,
			Timeout:     &timeout,
			Env:         map[string]string{"B": "override", "C": "job"},
		},
	}
	proj := &project.Config{
		ProjectName: "proj",
		WorkingDir:  workDir,
		CmdTemplate: "python train.py --lr {{lr}}",
		Defaults: project.Defaults{
			GPUsPerTask: 1,
			MaxRetry:    1,
		},
		Environment: map[string]string{"A": "project", "B": "project"},
		Resume:      project.ResumeConfig{Enabled: true, ExtraArgs: "--resume"},
		Wandb:       &project.WandbConfig{Project: "wandb-proj"},
	}

	plan, err := Build(context.Background(), cfg, proj, Deps{
		IDGen: nextID,
		Paths: Paths{
			WorkspaceRoot: filepath.Join(workDir, ".runq"),
			LogRoot:       filepath.Join(workDir, "logs"),
		},
		SkipPreflight: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.JobID != "job1" || plan.Project != "proj" || plan.Description != "demo" {
		t.Fatalf("plan identity mismatch: %+v", plan)
	}
	if plan.GPUsPerTask != 2 {
		t.Fatalf("GPUsPerTask = %d", plan.GPUsPerTask)
	}
	if plan.Wandb == nil || plan.Wandb.Project != "wandb-proj" {
		t.Fatalf("wandb mismatch: %+v", plan.Wandb)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(plan.Tasks))
	}

	first := plan.Tasks[0]
	if first.TaskID != "task1" {
		t.Fatalf("TaskID = %q", first.TaskID)
	}
	if first.GPUsNeeded != 2 || first.MaxRetry != 3 || first.Timeout != 7200 {
		t.Fatalf("numeric fields mismatch: %+v", first)
	}
	if first.TaskDir != filepath.Join(workDir, ".runq", "task1") {
		t.Fatalf("TaskDir = %q", first.TaskDir)
	}
	if first.CheckpointDir != filepath.Join(first.TaskDir, "checkpoints") {
		t.Fatalf("CheckpointDir = %q", first.CheckpointDir)
	}
	if first.LogPath != filepath.Join(workDir, "logs", "task1.log") {
		t.Fatalf("LogPath = %q", first.LogPath)
	}
	if first.Env["A"] != "project" || first.Env["B"] != "override" || first.Env["C"] != "job" {
		t.Fatalf("Env = %#v", first.Env)
	}
	if !first.Resumable || first.ExtraArgs != "--resume" {
		t.Fatalf("resume fields mismatch: %+v", first)
	}
	if first.WorkingDir != workDir {
		t.Fatalf("WorkingDir = %q", first.WorkingDir)
	}
	if _, ok := first.Env["RUNQ_TASK_ID"]; ok {
		t.Fatalf("Plan Env must not contain RUNQ_*: %#v", first.Env)
	}
}

// When Deps.JobID is set, Build uses it verbatim and reserves IDGen for task
// ids only. This is what lets HPC root its workspace on the job id
// (~/.runq/<job_id>/<task_id>) before Build computes task dirs.
func TestBuildUsesInjectedJobID(t *testing.T) {
	workDir := t.TempDir()
	ids := []string{"task1", "task2"} // no job id consumed from IDGen
	nextID := func() string {
		if len(ids) == 0 {
			t.Fatalf("IDGen called too many times (job id should come from Deps.JobID)")
		}
		id := ids[0]
		ids = ids[1:]
		return id
	}
	cfg := job.JobConfig{
		Project: "proj",
		Sweep: []job.SweepBlock{{
			Method:     "grid",
			Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1, 0.2}}},
		}},
	}
	proj := &project.Config{
		ProjectName: "proj",
		WorkingDir:  workDir,
		CmdTemplate: "python train.py --lr {{lr}}",
	}

	jobRoot := filepath.Join(workDir, "JOBID")
	plan, err := Build(context.Background(), cfg, proj, Deps{
		JobID:         "JOBID",
		IDGen:         nextID,
		Paths:         Paths{WorkspaceRoot: jobRoot, LogRoot: jobRoot},
		SkipPreflight: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.JobID != "JOBID" {
		t.Fatalf("JobID = %q, want JOBID", plan.JobID)
	}
	if plan.Tasks[0].TaskID != "task1" || plan.Tasks[1].TaskID != "task2" {
		t.Fatalf("task ids mismatch: %q %q", plan.Tasks[0].TaskID, plan.Tasks[1].TaskID)
	}
	if plan.Tasks[0].TaskDir != filepath.Join(jobRoot, "task1") {
		t.Fatalf("TaskDir = %q", plan.Tasks[0].TaskDir)
	}
}
