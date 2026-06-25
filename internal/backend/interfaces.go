package backend

import (
	"context"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

// Backend is the uniform interface consumed by the dashboard HTTP server
// and CLI --json. Both daemon and HPC backends implement it.
type Backend interface {
	// Capabilities is the backend's self-description (pure data, no I/O).
	// It is the single source of truth for what this backend can do; the
	// server and both UIs must consult it instead of inferring from mode.
	Capabilities() Capabilities
	// RefreshJob forces a reconcile from external sources. Only meaningful
	// for poll-model backends; push-model backends return ErrNotSupported.
	RefreshJob(ctx context.Context, jobID string) error

	// ListJobs returns visible jobs. An empty project lists globally (jobs
	// of archived projects cascade-hidden); a project scope skips the
	// cascade — navigating into an archived project shows its jobs.
	ListJobs(ctx context.Context, project string) ([]JobSummary, error)
	GetJob(ctx context.Context, jobID string) (*JobDetail, error)
	CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error)
	GPUStatus(ctx context.Context) ([]GPUSlot, error)

	GetTask(ctx context.Context, taskID string) (*TaskView, error)
	TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) // returns all metric points
	KillTask(ctx context.Context, taskID string) error
	RetryTask(ctx context.Context, taskID string) error
	KillJob(ctx context.Context, jobID string) error
	PauseJob(ctx context.Context, jobID string) error
	ResumeJob(ctx context.Context, jobID string) error

	SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (jobID string, totalTasks int, err error)
	DryRun(ctx context.Context, cfg job.JobConfig) (*DryRunResult, error)
	// PreviewSubmit renders what WOULD be submitted (preview is truth, zero
	// side effects). Backends without the concept return ErrNotSupported.
	PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error)
	// Archive = hide from default lists, keep everything; reversible.
	// ListJobs returns visible jobs only; ListArchivedJobs the rest.
	ListArchivedJobs(ctx context.Context) ([]JobSummary, error)
	ArchiveJob(ctx context.Context, jobID string) error
	UnarchiveJob(ctx context.Context, jobID string) error
	ArchiveProject(ctx context.Context, name string) error
	UnarchiveProject(ctx context.Context, name string) error
	// ResolveNote previews the note template's resolution ({{version}} family
	// scan needs the backend's store) — submit's code path, never a frontend
	// simulation.
	ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error)
	// Clean removes tasks matching the given selectors and their on-disk
	// artifacts. opts.DryRun=true returns what would be cleaned without
	// deleting. Backends without local storage return ErrNotSupported.
	Clean(ctx context.Context, opts CleanOptions) (*CleanResult, error)

	// ThawTasks releases SDK-frozen (SIGSTOPped) tasks. owner scopes by
	// UID; force bypasses the per-task disk safety check. Returns
	// ErrNotSupported in HPC mode (freeze/thaw is daemon-only).
	ThawTasks(ctx context.Context, owner int, force bool) (*ThawResponse, error)

	GetProject(ctx context.Context, name string) (*project.Config, error)
	ListProjects(ctx context.Context) ([]ProjectSummary, error)
	MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error)
	CreateProject(ctx context.Context, cfg project.Config) error
	UpdateProject(ctx context.Context, cfg project.Config) error
	RenameProject(ctx context.Context, oldName, newName string) error
}

type SubmitOptions struct {
	SkipPreflight bool
}
