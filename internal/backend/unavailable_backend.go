package backend

import (
	"context"
	"fmt"

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

func (b *UnavailableBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) ListTasks(ctx context.Context, opts TaskListOptions) ([]TaskView, int, error) {
	return nil, 0, b.wrap()
}

func (b *UnavailableBackend) TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) MetricKeys(ctx context.Context, jobID string) ([]string, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*LogPage, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error) {
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

func (b *UnavailableBackend) DryRun(ctx context.Context, cfg job.JobConfig) (*DryRunResult, error) {
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

func (b *UnavailableBackend) RenameProject(ctx context.Context, oldName, newName string) error {
	return b.wrap()
}

func (b *UnavailableBackend) Clean(ctx context.Context, opts CleanOptions) (*CleanResult, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) ThawTasks(ctx context.Context, owner int, force bool) (*ThawResponse, error) {
	return nil, b.wrap()
}

func (b *UnavailableBackend) wrap() error {
	return fmt.Errorf("dashboard backend unavailable: %w", b.err)
}
