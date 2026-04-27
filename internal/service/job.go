package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

// JobService handles job-level operations.
// All mutations go through here so DB + Queue + Scheduler stay in sync.
type JobService struct {
	Store     *store.Store
	Queue     *scheduler.Queue
	Scheduler *scheduler.Scheduler
	Exec      *executor.Executor
	Registry  *project.Registry
	Pool      resource.Allocator
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
func (s *JobService) SubmitJob(ctx context.Context, jobCfg job.JobConfig) (string, int, error) {
	// Validate project exists.
	proj, err := s.Registry.Get(jobCfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q not found", jobCfg.Project)
	}

	// Expand sweep into parameter combinations.
	taskParams, err := job.Expand(&jobCfg)
	if err != nil {
		return "", 0, fmt.Errorf("sweep expansion failed: %w", err)
	}

	// Determine effective settings (job overrides > project defaults).
	gpusPerTask := proj.Defaults.GPUsPerTask
	maxRetry := proj.Defaults.MaxRetry
	if jobCfg.Overrides != nil {
		if jobCfg.Overrides.GPUsPerTask != nil {
			gpusPerTask = *jobCfg.Overrides.GPUsPerTask
		}
		if jobCfg.Overrides.MaxRetry != nil {
			maxRetry = *jobCfg.Overrides.MaxRetry
		}
	}
	if gpusPerTask <= 0 {
		gpusPerTask = 1
	}

	// A6: reject if gpus_per_task exceeds total available GPUs.
	if s.Pool != nil {
		total := s.Pool.TotalCount()
		if gpusPerTask > total {
			return "", 0, fmt.Errorf("gpus_per_task (%d) exceeds total GPUs (%d)", gpusPerTask, total)
		}
	}

	// Merge env: project env + job override env.
	env := make(map[string]string)
	for k, v := range proj.Environment {
		env[k] = v
	}
	if jobCfg.Overrides != nil {
		for k, v := range jobCfg.Overrides.Env {
			env[k] = v
		}
	}

	// Generate job ID and build task list.
	jobID := GenerateID()
	now := time.Now()
	tasks := make([]*scheduler.Task, 0, len(taskParams))
	for _, params := range taskParams {
		cmd, err := job.Render(proj.CmdTemplate, params)
		if err != nil {
			return "", 0, fmt.Errorf("render command failed: %w", err)
		}
		taskID := GenerateID()
		tasks = append(tasks, &scheduler.Task{
			ID:          taskID,
			JobID:       jobID,
			ProjectName: proj.ProjectName,
			Command:     cmd,
			Params:      params,
			GPUsNeeded:  gpusPerTask,
			MaxRetry:    maxRetry,
			LogPath:     filepath.Join(proj.WorkingDir, "logs", taskID+".log"),
			WorkingDir:  proj.WorkingDir,
			Env:         env,
			Resumable:   proj.Resume.Enabled,
			ExtraArgs:   proj.Resume.ExtraArgs,
		})
	}

	// Persist job + tasks atomically.
	cfgJSON, err := json.Marshal(jobCfg)
	if err != nil {
		return "", 0, fmt.Errorf("marshal job config: %w", err)
	}

	jobRow := store.JobRow{
		ID: jobID, ProjectName: jobCfg.Project, Description: jobCfg.Description,
		ConfigJSON: string(cfgJSON), Status: "pending", TotalTasks: len(tasks), CreatedAt: now,
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
			Resumable: t.Resumable, ExtraArgs: t.ExtraArgs, EnqueuedAt: now,
		})
	}

	if err := s.Store.InsertJobWithTasks(ctx, &jobRow, taskRows); err != nil {
		return "", 0, fmt.Errorf("persist job: %w", err)
	}

	// DB succeeded — push to in-memory Queue.
	s.Queue.PushBatch(tasks)
	return jobID, len(tasks), nil
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
	return s.Store.UpdateJobStatus(ctx, jobID, "running")
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
