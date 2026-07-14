package submitplan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	posixpath "path"
	"sort"
	"strings"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/preflight"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
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
	// PreflightDisableLocal skips the local-subprocess checks (imports,
	// pip) — wired from hpc `preflight_local: false` for strict login nodes.
	PreflightDisableLocal bool
	// PreflightScope labels where local checks ran (e.g. "on login node").
	PreflightScope string
	// PreflightFS is the filesystem preflight checks run against — the
	// TARGET's rfs.FS for remote targets (paths statted remotely, script
	// read remotely, probes executed on the login node). nil = local os.
	PreflightFS rfs.FS
	// SchedulerParams are param names consumed by the HPC submit_template
	// ({{param.*}}): exempt from the command renderer's unconsumed check
	// and excluded from {{args}} injection.
	SchedulerParams []string
}

type Plan struct {
	JobID       string
	Project     string
	Description string
	Note        string
	GPUsPerTask int
	Wandb       *project.WandbConfig
	Tasks       []PlannedTask
	SweepKeys   []string // swept parameter names (sorted), for WANDB_TAGS/RUN_NAME
	// Preflight is the full three-state report (failed entries already
	// aborted Build) — callers print it so skips stay visible.
	Preflight preflight.Report
}

type PlannedTask struct {
	TaskID        string
	Name          string // scheduler job name ({{name}} in submit_template), pre-sanitized
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

// validateStrictChoices enforces strict params: a value outside the
// project's Choices list is a submit-time error. Comparison is on the
// string form (choices are stored as strings; params may be typed).
func validateStrictChoices(proj *project.Config, tasks []job.TaskParams) error {
	for _, def := range proj.Params {
		if !def.Strict || len(def.Choices) == 0 {
			continue
		}
		allowed := make(map[string]bool, len(def.Choices))
		for _, c := range def.Choices {
			allowed[c] = true
		}
		for _, params := range tasks {
			v, present := params[def.Name]
			if !present {
				continue
			}
			s := fmt.Sprintf("%v", v)
			if !allowed[s] {
				return fmt.Errorf(
					"param %q is strict: value %q is not among its choices (%s)",
					def.Name, s, strings.Join(def.Choices, ", "))
			}
		}
	}
	return nil
}

// resolveEnvFile applies the project's env_file semantics: nil = auto-use
// <working_dir>/.env when present; "" = disabled; other = required path.
// Existence questions go to fsys — the TARGET's filesystem (RQ-65): the
// .env lives next to the code, and the code lives on the target.
func resolveEnvFile(proj *project.Config, fsys rfs.FS) (string, error) {
	if proj.EnvFile == nil {
		auto := posixpath.Join(proj.WorkingDir, ".env")
		if _, err := fsys.Stat(auto); err == nil {
			return auto, nil
		}
		return "", nil
	}
	path := strings.TrimSpace(*proj.EnvFile)
	if path == "" {
		return "", nil // explicitly disabled
	}
	if !posixpath.IsAbs(path) {
		path = posixpath.Join(proj.WorkingDir, path)
	}
	if _, err := fsys.Stat(path); err != nil {
		return "", fmt.Errorf("env_file %q: %w", path, err)
	}
	return path, nil
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
	// Contract boundary: only -1 means unlimited. Validated AFTER the
	// override merge so a hand-written job.yaml can't smuggle an arbitrary
	// negative past a clean project config.
	if err := project.ValidateRetryBounds(maxRetry); err != nil {
		return Plan{}, err
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

	// working_dir existence is the TARGET filesystem's question (RQ-65) —
	// a Mac daemon statting a TSUBAME path answers about the wrong world.
	fsys := d.PreflightFS
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	if _, err := fsys.Stat(proj.WorkingDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Plan{}, fmt.Errorf("working_dir %q does not exist on target", proj.WorkingDir)
		}
		return Plan{}, fmt.Errorf("stat working_dir %q: %w", proj.WorkingDir, err)
	}

	// Ambient env file: carried as the reserved RUNQ_ENV_FILE env key so it
	// flows through EnvJSON persistence / daemon recovery untouched. The
	// shell sources it AT TASK START (executor prologue / run.sh header) —
	// runq never reads its values. Precedence: .env < explicit env.
	envFile, err := resolveEnvFile(proj, fsys)
	if err != nil {
		return Plan{}, err
	}
	if envFile != "" {
		env["RUNQ_ENV_FILE"] = envFile
	}

	if err := validateStrictChoices(proj, taskParams); err != nil {
		return Plan{}, err
	}

	// Scheduler params: DECLARED scope wins (self-describing config);
	// template-reference inference remains as a fallback for configs that
	// haven't adopted scope yet.
	schedParams := make(map[string]bool, len(d.SchedulerParams))
	for _, n := range d.SchedulerParams {
		schedParams[n] = true
	}
	for _, def := range proj.Params {
		if def.Scope == "scheduler" {
			schedParams[def.Name] = true
		}
	}

	pf := preflight.DefaultPreflight()
	pf.Skip = d.SkipPreflight
	pf.DisableLocal = d.PreflightDisableLocal
	pf.Scope = d.PreflightScope
	pf.ExcludeParams = schedParams
	pf.FS = d.PreflightFS
	pfReport, err := pf.Run(ctx, proj, taskParams)
	if err != nil {
		return Plan{}, err
	}
	if err := pfReport.Err(); err != nil {
		return Plan{}, err
	}

	jobID := d.JobID
	if jobID == "" {
		jobID = utils.GenerateJobID()
	}
	callerUID := os.Getuid()
	// Scheduler job name template: job.yaml override > project.yaml job_name
	// > default rq-{{task_id}}. Rendered per task (params differ), always
	// sanitized — qsub/sbatch must never reject what runq generated.
	nameTmpl := cfg.Name
	if strings.TrimSpace(nameTmpl) == "" {
		nameTmpl = proj.JobName
	}
	tasks := make([]PlannedTask, 0, len(taskParams))
	for _, params := range taskParams {
		cmd, err := job.RenderExcluding(proj.CmdTemplate, params, schedParams)
		if err != nil {
			return Plan{}, fmt.Errorf("render command failed: %w", err)
		}
		cmd = utils.WrapCommand(proj.PythonEnv.Type, proj.PythonEnv.Path, proj.PythonEnv.Name, cmd, proj.WorkingDir)

		taskID := idGen()
		taskDir := workspace.TaskDir(d.Paths.WorkspaceRoot, taskID)
		name := job.RenderJobName(nameTmpl, params, map[string]string{
			"project": cfg.Project, "job_id": jobID, "task_id": taskID,
		})
		tasks = append(tasks, PlannedTask{
			TaskID:        taskID,
			Name:          name,
			Command:       cmd,
			Params:        params,
			Env:           cloneEnv(env),
			GPUsNeeded:    gpusPerTask,
			MaxRetry:      maxRetry,
			Timeout:       timeoutSec,
			LogPath:       posixpath.Join(d.Paths.LogRoot, taskID+".log"),
			WorkingDir:    proj.WorkingDir,
			Resumable:     proj.Resume.Enabled,
			ExtraArgs:     proj.Resume.ExtraArgs,
			UID:           callerUID,
			TaskDir:       taskDir,
			CheckpointDir: workspace.CheckpointsDir(taskDir),
		})
	}

	// Derive sweep keys: unique parameter names across all sweep blocks.
	var sweepKeys []string
	seenKey := make(map[string]bool)
	for _, block := range cfg.Sweep {
		for k := range block.Parameters {
			if !seenKey[k] {
				seenKey[k] = true
				sweepKeys = append(sweepKeys, k)
			}
		}
	}
	sort.Strings(sweepKeys)

	return Plan{
		JobID:       jobID,
		Project:     cfg.Project,
		Description: cfg.Description,
		Note:        cfg.Note,
		GPUsPerTask: gpusPerTask,
		Wandb:       proj.Wandb,
		Tasks:       tasks,
		SweepKeys:   sweepKeys,
		Preflight:   pfReport,
	}, nil
}

func cloneEnv(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
