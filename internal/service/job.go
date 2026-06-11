package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/submitplan"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gliese129/runq/internal/workspace"
)

// JobService handles job-level operations.
// All mutations go through here so DB + Queue + Scheduler stay in sync.
type JobService struct {
	Store      *store.Store
	Queue      *scheduler.Queue
	Scheduler  *scheduler.Scheduler
	Exec       *executor.Executor
	Registry   *project.Registry
	Pool       resource.Allocator
	StorageCfg *config.GlobalConfig // nil-safe: nil = project_path mode
}

// SubmitJobOpts carries per-call overrides for SubmitJob. Today the
// only knob is whether to skip preflight; future per-call flags
// (e.g. priority) land here so the function signature doesn't bloat.
type SubmitJobOpts struct {
	SkipPreflight bool
}

// ResolveNote renders note placeholders for preview — same code path the
// submit uses, so the GUI's "will be submitted as ..." is truth, not a
// frontend simulation (U1).
func (s *JobService) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	return resolveNote(ctx, s.Store, cfg)
}

// resolveNote renders note placeholders against this project's existing
// job notes ({{version}} family scan needs them).
func resolveNote(ctx context.Context, st *store.Store, cfg job.JobConfig) (string, error) {
	rows, err := st.ListJobs(ctx, cfg.Project)
	if err != nil {
		return "", fmt.Errorf("list jobs for note rendering: %w", err)
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		notes = append(notes, r.Note)
	}
	return job.RenderNote(&cfg, job.NoteContext{
		Project: cfg.Project, Now: time.Now(), ExistingNotes: notes,
	})
}

// JobSummary is the API response for job listing.
type JobSummary struct {
	JobID       string         `json:"job_id"`
	Project     string         `json:"project"`
	Status      string         `json:"status"`
	TotalTasks  int            `json:"total_tasks"`
	StatusCount map[string]int `json:"status_count"`
	CreatedAt   time.Time      `json:"created_at"`
}

// JobDetail is the API response for job show.
type JobDetail struct {
	Job   *store.JobRow   `json:"job"`
	Tasks []store.TaskRow `json:"tasks"`
}

// SubmitJob validates, expands, persists, and enqueues a job.
// Returns the job ID and total task count.
//
// Calls preflight (F8 — fail-before-queue checks) after sweep expansion
// but before any DB writes, so an invalid env / missing path / broken
// import surfaces at submit time rather than after a 2-hour queue wait.
// Use SubmitJobWithOpts to pass per-call options such as --no-preflight.
func (s *JobService) SubmitJob(ctx context.Context, jobCfg job.JobConfig) (string, int, error) {
	return s.SubmitJobWithOpts(ctx, jobCfg, SubmitJobOpts{})
}

// SubmitJobWithOpts is SubmitJob plus the per-call options struct.
// Lives as a separate symbol so existing callers (tests, internal
// schedulers) don't have to learn the new arg.
func (s *JobService) SubmitJobWithOpts(ctx context.Context, jobCfg job.JobConfig, opts SubmitJobOpts) (string, int, error) {
	// Validate project exists.
	proj, err := s.Registry.Get(jobCfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q not found", jobCfg.Project)
	}

	wsRoot, err := config.ResolveRoot(s.StorageCfg, proj.WorkingDir, proj.ProjectName)
	if err != nil {
		return "", 0, fmt.Errorf("resolve workspace root: %w", err)
	}

	jobID := utils.GenerateID()
	jobRoot := filepath.Join(wsRoot, jobID)

	// Resolve note placeholders ({{version}}, {{date}}, params, ...) for
	// display: the plan carries the RESOLVED note (job row, workspace files),
	// while ConfigJSON below keeps the original template so re-submitting
	// the same config keeps incrementing {{version}}.
	planCfg := jobCfg
	if resolved, nerr := resolveNote(ctx, s.Store, jobCfg); nerr != nil {
		return "", 0, nerr
	} else {
		planCfg.Note = resolved
	}

	plan, err := submitplan.Build(ctx, planCfg, proj, submitplan.Deps{
		JobID: jobID,
		IDGen: utils.GenerateID,
		Paths: submitplan.Paths{
			WorkspaceRoot: jobRoot,
			LogRoot:       jobRoot,
		},
		SkipPreflight: opts.SkipPreflight,
	})
	if err != nil {
		return "", 0, err
	}

	// One-shot job setup (e.g. dataset/model pre-download). Runs before any
	// DB row exists — failure leaves nothing behind.
	if err := submitplan.RunSetup(ctx, proj, jobCfg); err != nil {
		return "", 0, err
	}

	// A6: reject if gpus_per_task exceeds total available GPUs.
	if s.Pool != nil {
		total := s.Pool.TotalCount()
		if plan.GPUsPerTask > total {
			return "", 0, fmt.Errorf("gpus_per_task (%d) exceeds total GPUs (%d)", plan.GPUsPerTask, total)
		}
	}

	now := time.Now()
	tasks := make([]*scheduler.Task, 0, len(plan.Tasks))
	for _, planned := range plan.Tasks {
		// L2-C: per-task workspace. Created BEFORE the DB insert so that if mkdir
		// fails, no half-state is persisted. Residual dirs on later failures are
		// tolerated — the next submission uses a fresh task_id.
		if err := workspace.Write(planned.TaskDir, planned.Params, plan.Wandb, plan.Note); err != nil {
			return "", 0, fmt.Errorf("prepare task workspace for %s: %w", planned.TaskID, err)
		}

		tasks = append(tasks, &scheduler.Task{
			ID:            planned.TaskID,
			JobID:         plan.JobID,
			ProjectName:   plan.Project,
			Command:       planned.Command,
			Params:        planned.Params,
			GPUsNeeded:    planned.GPUsNeeded,
			MaxRetry:      planned.MaxRetry,
			LogPath:       planned.LogPath,
			WorkingDir:    planned.WorkingDir,
			Env:           planned.Env,
			Resumable:     planned.Resumable,
			ExtraArgs:     planned.ExtraArgs,
			Timeout:       planned.Timeout,
			UID:           planned.UID,
			TaskDir:       planned.TaskDir,
			CheckpointDir: planned.CheckpointDir,
		})
	}

	// Persist job + tasks atomically.
	cfgJSON, err := json.Marshal(jobCfg)
	if err != nil {
		return "", 0, fmt.Errorf("marshal job config: %w", err)
	}

	jobRow := store.JobRow{
		ID: plan.JobID, ProjectName: plan.Project, Description: plan.Description,
		Note: plan.Note, ConfigJSON: string(cfgJSON), Status: "pending", TotalTasks: len(plan.Tasks), CreatedAt: now,
	}
	taskRows := make([]store.TaskRow, 0, len(tasks))
	for _, t := range tasks {
		paramsJSON, _ := json.Marshal(t.Params)
		envJSON, _ := json.Marshal(t.Env)
		taskRows = append(taskRows, store.TaskRow{
			ID: t.ID, JobID: t.JobID, ProjectName: t.ProjectName,
			Command: t.Command, ParamsJSON: string(paramsJSON), GPUsNeeded: t.GPUsNeeded,
			Status: "pending", MaxRetry: t.MaxRetry, LogPath: t.LogPath,
			WorkingDir: t.WorkingDir, EnvJSON: string(envJSON),
			Resumable: t.Resumable, ExtraArgs: t.ExtraArgs,
			UID: t.UID, Timeout: t.Timeout, EnqueuedAt: now,
			TaskDir: t.TaskDir,
		})
	}

	if err := s.Store.InsertJobWithTasks(ctx, &jobRow, taskRows); err != nil {
		return "", 0, fmt.Errorf("persist job: %w", err)
	}

	// DB succeeded — push to in-memory Queue.
	s.Queue.PushBatch(tasks)
	return plan.JobID, len(tasks), nil
}

// ListJobs returns a summary of all jobs with task status breakdown.
func (s *JobService) ListJobs(ctx context.Context, projectFilter string) ([]JobSummary, error) {
	jobs, err := s.Store.ListJobs(ctx, projectFilter)
	if err != nil {
		return nil, err
	}

	results := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		tasks, _ := s.Store.ListTasks(ctx, store.TaskFilter{JobID: j.ID})
		counts := map[string]int{"pending": 0, "running": 0, "success": 0, "failed": 0, "killed": 0}
		for _, t := range tasks {
			counts[t.Status]++
		}
		results = append(results, JobSummary{
			JobID: j.ID, Project: j.ProjectName, Status: j.Status,
			TotalTasks: j.TotalTasks, StatusCount: counts, CreatedAt: j.CreatedAt,
		})
	}
	return results, nil
}

// ShowJob returns full job details with all tasks.
func (s *JobService) ShowJob(ctx context.Context, jobID string) (*JobDetail, error) {
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	tasks, err := s.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return &JobDetail{Job: j, Tasks: tasks}, nil
}

// KillJob kills all running/pending tasks in a job. Returns count of affected tasks.
func (s *JobService) KillJob(ctx context.Context, jobID string) (int, error) {
	tasks := s.Queue.ListByJob(jobID)
	if len(tasks) == 0 {
		return 0, fmt.Errorf("job %q not found", jobID)
	}

	killed := 0
	for _, t := range tasks {
		if t.Status == scheduler.StatusRunning {
			s.Scheduler.RequestKill(t.ID)
			s.Exec.Stop(t.ID)
			killed++
		} else if t.Status == scheduler.StatusPending {
			_ = s.Queue.Complete(t.ID, scheduler.StatusKilled)
			_ = s.Store.UpdateTaskStatus(ctx, t.ID, "killed", map[string]any{
				"finished_at": time.Now().Unix(),
			})
			killed++
		}
	}
	if err := s.refreshJobStatus(ctx, jobID); err != nil {
		return killed, err
	}
	return killed, nil
}

func (s *JobService) refreshJobStatus(ctx context.Context, jobID string) error {
	tasks, err := s.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}

	counts := map[string]int{"running": 0, "pending": 0, "done": 0}
	for _, t := range tasks {
		switch t.Status {
		case "running":
			counts["running"]++
		case "pending":
			counts["pending"]++
		case "success", "failed", "killed":
			counts["done"]++
		}
	}

	isStarted := (counts["running"] + counts["done"]) > 0
	isEnded := (counts["pending"] + counts["running"]) == 0

	var newStatus string
	if isEnded {
		newStatus = "done"
	} else if isStarted {
		newStatus = "running"
	} else {
		newStatus = "pending"
	}

	return s.Store.UpdateJobStatus(ctx, jobID, newStatus)
}

// PauseJob pauses a job — scheduler skips its pending tasks.
func (s *JobService) PauseJob(ctx context.Context, jobID string) error {
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", jobID)
	}
	if j.Status == "done" {
		return fmt.Errorf("job %q is already done", jobID)
	}
	s.Scheduler.PauseJob(jobID)
	return s.Store.UpdateJobStatus(ctx, jobID, "paused")
}

// ResumeJob resumes a paused job.
func (s *JobService) ResumeJob(ctx context.Context, jobID string) error {
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", jobID)
	}
	if j.Status != "paused" {
		return fmt.Errorf("job %q is %s, not paused", jobID, j.Status)
	}
	s.Scheduler.ResumeJob(jobID)
	// Derive correct status from actual task states instead of hardcoding "running".
	// e.g. if all tasks are still pending, job should be "pending" not "running".
	s.Scheduler.RefreshJobStatus(jobID)
	return nil
}

// RemoveJob deletes a completed job and its tasks from DB.
func (s *JobService) RemoveJob(ctx context.Context, jobID string) error {
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %q not found", jobID)
	}
	if j.Status != "done" {
		return fmt.Errorf("job %q is %s, only completed jobs can be removed", jobID, j.Status)
	}
	return s.Store.DeleteJob(ctx, jobID)
}
