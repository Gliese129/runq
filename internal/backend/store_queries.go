package backend

import (
	"context"
	"fmt"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
)

// storeQueries groups the store + registry fields and the Backend methods
// that are identical across LocalBackend and SSHBackend: pure delegation
// to the project registry, store-level clean, and dry-run.
//
// Embed this in concrete backends to avoid duplicating ~11 methods.
type storeQueries struct {
	store *store.Store
	reg   *project.Registry
}

// setProjectRegistry swaps in the multi-backend's routed registry (RQ-65)
// so every lane resolves project yaml on the project's home filesystem.
func (b *storeQueries) setProjectRegistry(reg *project.Registry) { b.reg = reg }

// ── Project CRUD ─────────────────────────────────────────────────────────

func (b *storeQueries) GetProject(ctx context.Context, name string) (*project.Config, error) {
	return b.reg.Get(ctx, name)
}

func (b *storeQueries) CreateProject(ctx context.Context, cfg project.Config) error {
	return b.reg.Add(ctx, cfg)
}

func (b *storeQueries) UpdateProject(ctx context.Context, cfg project.Config) error {
	return b.reg.Update(ctx, cfg)
}

func (b *storeQueries) RenameProject(ctx context.Context, oldName, newName string) error {
	return b.reg.Rename(ctx, oldName, newName)
}

func (b *storeQueries) ArchiveProject(ctx context.Context, name string) error {
	return b.reg.Archive(ctx, name)
}

func (b *storeQueries) UnarchiveProject(ctx context.Context, name string) error {
	return b.reg.Unarchive(ctx, name)
}

// ── MatchProjects ────────────────────────────────────────────────────────

func (b *storeQueries) MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error) {
	configs, err := b.reg.Match(ctx, dir)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

// configsToSummaries converts a slice of project configs into ProjectSummary
// values, enriching with job counts and archive status from the store.
func (b *storeQueries) configsToSummaries(ctx context.Context, configs []project.Config) ([]ProjectSummary, error) {
	jobs, _ := b.store.ListJobs(ctx, "", "")
	jobCounts := make(map[string]int, len(configs))
	for _, j := range jobs {
		jobCounts[j.ProjectName]++
	}
	archived, _ := b.reg.ArchivedNames(ctx)
	out := make([]ProjectSummary, 0, len(configs))
	for _, c := range configs {
		out = append(out, ProjectSummary{
			Name:     c.ProjectName,
			WorkDir:  c.WorkingDir,
			JobCount: jobCounts[c.ProjectName],
			Archived: archived[c.ProjectName],
		})
	}
	return out, nil
}

// ── DryRun ───────────────────────────────────────────────────────────────

func (b *storeQueries) DryRun(ctx context.Context, cfg job.JobConfig) (*DryRunResult, error) {
	return BuildDryRunResult(cfg, func(name string) (*project.Config, error) {
		return b.reg.Get(ctx, name)
	}, nil)
}

// ── Clean / Thaw ─────────────────────────────────────────────────────────

func (b *storeQueries) Clean(ctx context.Context, opts CleanOptions) (*CleanResult, error) {
	// Single-lane path (runqd's ledger, tests): everything is local.
	// Cross-target FS routing lives in MultiBackend.Clean.
	return PerformClean(ctx, b.store, nil, opts)
}

func (b *storeQueries) ThawTasks(_ context.Context, _ int, _ bool) (*ThawResponse, error) {
	return nil, fmt.Errorf("thaw tasks: %w", ErrNotSupported)
}
