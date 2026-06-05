package submitplan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/preflight"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gliese129/runq/internal/workspace"
)

// Paths are backend-selected roots. Build only joins below these roots; it does
// not decide where daemon or HPC deployments should place their files.
type Paths struct {
	WorkspaceRoot string
	LogRoot       string
}

type Deps struct {
	// JobID is supplied by the backend, not generated here. The backend roots
	// its workspace on the job id (HPC uses ~/.runq/<job_id>/<task_id>), so it
	// must know the id before Build computes task dirs. When empty, Build falls
	// back to IDGen for backward compatibility.
	JobID         string
	IDGen         func() string // used for task ids (and job id only when JobID is empty)
	Paths         Paths
	SkipPreflight bool
}

type Plan struct {
	JobID       string
	Project     string
	Description string
	Note        string
	GPUsPerTask int
	Wandb       *project.WandbConfig
	Tasks       []PlannedTask
}

type PlannedTask struct {
	TaskID        string
	Command       string
	Params        job.TaskParams
	Env           map[string]string
	GPUsNeeded    int
	MaxRetry      int
	Timeout       int
	LogPath       string
	WorkingDir    string
	Resumable     bool
	ExtraArgs     string
	UID           int
	TaskDir       string
	CheckpointDir string
}

// Build compiles a JobConfig plus an already-resolved Project into a
// backend-neutral plan. It has no persistent side effects: no workspace
// creation, no DB writes, no scheduler/queue interaction.
func Build(ctx context.Context, cfg job.JobConfig, proj *project.Config, d Deps) (Plan, error) {
	if proj == nil {
		return Plan{}, fmt.Errorf("project config is nil")
	}
	idGen := d.IDGen
	if idGen == nil {
		idGen = utils.GenerateID
	}

	taskParams, err := job.Expand(&cfg)
	if err != nil {
		return Plan{}, fmt.Errorf("sweep expansion failed: %w", err)
	}

	gpusPerTask := proj.Defaults.GPUsPerTask
	maxRetry := proj.Defaults.MaxRetry
	if cfg.Overrides != nil {
		if cfg.Overrides.GPUsPerTask != nil {
			gpusPerTask = *cfg.Overrides.GPUsPerTask
		}
		if cfg.Overrides.MaxRetry != nil {
			maxRetry = *cfg.Overrides.MaxRetry
		}
	}
	if gpusPerTask < 0 {
		gpusPerTask = 0
	}

	var timeoutSec int
	timeoutStr := proj.Defaults.Timeout
	if cfg.Overrides != nil && cfg.Overrides.Timeout != nil {
		timeoutStr = *cfg.Overrides.Timeout
	}
	if timeoutStr != "" {
		duration, err := utils.ParseHumanDuration(timeoutStr)
		if err != nil {
			return Plan{}, fmt.Errorf("invalid timeout %q: %w", timeoutStr, err)
		}
		timeoutSec = int(duration.Seconds())
	}

	env := make(map[string]string)
	for k, v := range proj.Environment {
		env[k] = v
	}
	if cfg.Overrides != nil {
		for k, v := range cfg.Overrides.Env {
			env[k] = v
		}
	}

	if _, err := os.Stat(proj.WorkingDir); err != nil {
		if os.IsNotExist(err) {
			return Plan{}, fmt.Errorf("working_dir %q does not exist", proj.WorkingDir)
		}
		return Plan{}, fmt.Errorf("stat working_dir %q: %w", proj.WorkingDir, err)
	}

	if err := preflight.Run(ctx, proj, taskParams, d.SkipPreflight); err != nil {
		return Plan{}, err
	}

	jobID := d.JobID
	if jobID == "" {
		jobID = idGen()
	}
	callerUID := os.Getuid()
	tasks := make([]PlannedTask, 0, len(taskParams))
	for _, params := range taskParams {
		cmd, err := job.Render(proj.CmdTemplate, params)
		if err != nil {
			return Plan{}, fmt.Errorf("render command failed: %w", err)
		}
		cmd = utils.WrapCommand(proj.PythonEnv.Type, proj.PythonEnv.Path, proj.PythonEnv.Name, cmd, proj.WorkingDir)

		taskID := idGen()
		taskDir := workspace.TaskDir(d.Paths.WorkspaceRoot, taskID)
		tasks = append(tasks, PlannedTask{
			TaskID:        taskID,
			Command:       cmd,
			Params:        params,
			Env:           cloneEnv(env),
			GPUsNeeded:    gpusPerTask,
			MaxRetry:      maxRetry,
			Timeout:       timeoutSec,
			LogPath:       filepath.Join(d.Paths.LogRoot, taskID+".log"),
			WorkingDir:    proj.WorkingDir,
			Resumable:     proj.Resume.Enabled,
			ExtraArgs:     proj.Resume.ExtraArgs,
			UID:           callerUID,
			TaskDir:       taskDir,
			CheckpointDir: workspace.CheckpointsDir(taskDir),
		})
	}

	return Plan{
		JobID:       jobID,
		Project:     cfg.Project,
		Description: cfg.Description,
		Note:        cfg.Note,
		GPUsPerTask: gpusPerTask,
		Wandb:       proj.Wandb,
		Tasks:       tasks,
	}, nil
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
