package submitplan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/project"
)

func TestBuildDeterministicPlan(t *testing.T) {
	workDir := t.TempDir()
	ids := []string{"task1", "task2"}
	nextID := func() string {
		if len(ids) == 0 {
			t.Fatalf("IDGen called too many times (job id should come from Deps.JobID)")
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

	jobRoot := filepath.Join(workDir, ".runq", "job1")
	plan, err := Build(context.Background(), cfg, proj, Deps{
		JobID: "job1",
		IDGen: nextID,
		Paths: Paths{
			WorkspaceRoot: jobRoot,
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
	if first.TaskDir != filepath.Join(jobRoot, "task1") {
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

// P4: env_file semantics — nil auto-detects working_dir/.env; the reserved
// RUNQ_ENV_FILE key carries it; explicit-missing errors; "" disables.
func TestEnvFileResolution(t *testing.T) {
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo {{lr}}"}
	cfg := job.JobConfig{Project: "p", Sweep: []job.SweepBlock{{Method: "grid",
		Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1}}}}}}
	deps := Deps{Paths: Paths{WorkspaceRoot: dir, LogRoot: dir}, SkipPreflight: true}

	// auto, no .env present → key absent
	plan, err := Build(context.Background(), cfg, proj, deps)
	if err != nil {
		t.Fatal(err)
	}
	if v := plan.Tasks[0].Env["RUNQ_ENV_FILE"]; v != "" {
		t.Errorf("no .env: key should be absent, got %q", v)
	}

	// auto, .env present → picked up
	envPath := filepath.Join(dir, ".env")
	os.WriteFile(envPath, []byte("A=1\n"), 0o600)
	plan, err = Build(context.Background(), cfg, proj, deps)
	if err != nil {
		t.Fatal(err)
	}
	if v := plan.Tasks[0].Env["RUNQ_ENV_FILE"]; v != envPath {
		t.Errorf("auto .env: got %q want %q", v, envPath)
	}

	// explicitly disabled
	empty := ""
	proj.EnvFile = &empty
	plan, _ = Build(context.Background(), cfg, proj, deps)
	if v := plan.Tasks[0].Env["RUNQ_ENV_FILE"]; v != "" {
		t.Errorf("disabled: key should be absent, got %q", v)
	}

	// explicit missing path → error
	missing := "nope.env"
	proj.EnvFile = &missing
	if _, err := Build(context.Background(), cfg, proj, deps); err == nil {
		t.Error("explicit missing env_file must error")
	}
}

// Strict choices: values outside the catalog are a submit-time error;
// non-strict choices stay suggestions.
func TestStrictChoices(t *testing.T) {
	dir := t.TempDir()
	proj := &project.Config{
		ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo {{provider}}",
		Params: []project.ParamDef{
			{Name: "provider", Type: "str", Choices: []string{"vllm", "openai"}, Strict: true},
		},
	}
	deps := Deps{Paths: Paths{WorkspaceRoot: dir, LogRoot: dir}, SkipPreflight: true}
	cfgFor := func(v string) job.JobConfig {
		return job.JobConfig{Project: "p", Sweep: []job.SweepBlock{{Method: "grid",
			Parameters: map[string]job.ParameterSpec{"provider": {Values: []any{v}}}}}}
	}

	if _, err := Build(context.Background(), cfgFor("vllm"), proj, deps); err != nil {
		t.Fatalf("allowed value rejected: %v", err)
	}
	_, err := Build(context.Background(), cfgFor("deepinfra"), proj, deps)
	if err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("strict violation should error with explanation, got %v", err)
	}

	proj.Params[0].Strict = false
	if _, err := Build(context.Background(), cfgFor("deepinfra"), proj, deps); err != nil {
		t.Fatalf("non-strict choices must stay suggestions: %v", err)
	}
}

// Declared scope beats inference: a scheduler-scoped param needs no
// submit_template knowledge to be exempt from command consumption.
func TestSchedulerScopeDeclaration(t *testing.T) {
	dir := t.TempDir()
	proj := &project.Config{
		ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo {{lr}}",
		Params: []project.ParamDef{{Name: "h_rt", Type: "str", Scope: "scheduler"}},
	}
	cfg := job.JobConfig{Project: "p",
		FixedParams: map[string]any{"h_rt": "8:00:00"},
		Sweep: []job.SweepBlock{{Method: "grid",
			Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1}}}}}}
	deps := Deps{Paths: Paths{WorkspaceRoot: dir, LogRoot: dir}, SkipPreflight: true}

	plan, err := Build(context.Background(), cfg, proj, deps)
	if err != nil {
		t.Fatalf("declared scheduler param must not require command consumption: %v", err)
	}
	if strings.Contains(plan.Tasks[0].Command, "h_rt") {
		t.Errorf("scheduler param leaked into command: %q", plan.Tasks[0].Command)
	}
}

// Regression: presets used -N {{task_id}} and hex task ids can start with a
// digit — UGE rejects ("not a valid object name"). Every planned task must
// carry a scheduler-safe Name; templates resolve from job.name > project
// job_name > rq-{{task_id}}.
func TestPlannedTaskName(t *testing.T) {
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo {{lr}}"}
	cfg := job.JobConfig{Project: "p",
		Sweep: []job.SweepBlock{{Method: "grid",
			Parameters: map[string]job.ParameterSpec{"lr": {Values: []any{0.1}}}}}}
	deps := Deps{Paths: Paths{WorkspaceRoot: dir, LogRoot: dir}, SkipPreflight: true}

	// default: rq-<task_id> — never digit-first
	plan, err := Build(context.Background(), cfg, proj, deps)
	if err != nil {
		t.Fatal(err)
	}
	if want := "rq-" + plan.Tasks[0].TaskID; plan.Tasks[0].Name != want {
		t.Errorf("default name: got %q want %q", plan.Tasks[0].Name, want)
	}

	// project template with params; job.yaml override wins
	proj.JobName = "eval-{{lr}}"
	plan, err = Build(context.Background(), cfg, proj, deps)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].Name != "eval-0.1" {
		t.Errorf("project template: got %q", plan.Tasks[0].Name)
	}
	cfg.Name = "{{project}}-x"
	plan, err = Build(context.Background(), cfg, proj, deps)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].Name != "p-x" {
		t.Errorf("job override: got %q", plan.Tasks[0].Name)
	}
}
