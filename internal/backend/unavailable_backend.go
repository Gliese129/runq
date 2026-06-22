package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

type UnavailableBackend struct {
	err error
}

func NewUnavailableBackend(err error) *UnavailableBackend {
	return &UnavailableBackend{err: err}
}

// Capabilities: everything false — an unreachable backend honestly has no
// capabilities. StateModel "push" keeps the GUI from rendering staleness UI
// on top of the connection-error state.
func (b *UnavailableBackend) Capabilities() Capabilities {
	return Capabilities{StateModel: "push"}
}

func (b *UnavailableBackend) RefreshJob(ctx context.Context, jobID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	return "", b.wrap()
}

func (b *UnavailableBackend) ListJobs(ctx context.Context, project string) ([]JobSummary, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) GetTask(ctx context.Context, taskID string) (*TaskView, string, error) {
	return nil, "", b.wrap()
}

func (b *UnavailableBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) KillTask(ctx context.Context, taskID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) RetryTask(ctx context.Context, taskID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) KillJob(ctx context.Context, jobID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) PauseJob(ctx context.Context, jobID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) ResumeJob(ctx context.Context, jobID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	return "", 0, b.wrap()
}

func (b *UnavailableBackend) ListArchivedJobs(context.Context) ([]JobSummary, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) ArchiveJob(context.Context, string) error { return b.wrap() }

func (b *UnavailableBackend) UnarchiveJob(context.Context, string) error { return b.wrap() }

func (b *UnavailableBackend) ArchiveProject(context.Context, string) error { return b.wrap() }

func (b *UnavailableBackend) UnarchiveProject(context.Context, string) error { return b.wrap() }

func (b *UnavailableBackend) PreviewSubmit(context.Context, job.JobConfig, bool) (string, error) {
	return "", b.wrap()
}

func (b *UnavailableBackend) DryRun(ctx context.Context, cfg job.JobConfig) ([]job.TaskParams, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) GetProject(ctx context.Context, name string) (*project.Config, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) CreateProject(ctx context.Context, cfg project.Config) error {
	return b.wrap()
}

func (b *UnavailableBackend) UpdateProject(ctx context.Context, cfg project.Config) error {
	return b.wrap()
}

func (b *UnavailableBackend) DeleteProject(ctx context.Context, name string) error {
	return b.wrap()
}

func (b *UnavailableBackend) RenameProject(ctx context.Context, oldName, newName string) error {
	return b.wrap()
}

func (b *UnavailableBackend) DeleteJob(ctx context.Context, jobID string) error {
	return b.wrap()
}

func (b *UnavailableBackend) CleanOldTasks(ctx context.Context, cutoff time.Time, dryRun bool) (*CleanResult, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) wrap() error {
	return fmt.Errorf("dashboard backend unavailable: %w", b.err)
}
