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
	// ListTasks is the flat cross-job task table (spec §5.5, D7 — `runq ps
	// <job_id>` and the WebUI task list). Pagination is SQL-level (D20);
	// total is the unpaginated match count.
	ListTasks(ctx context.Context, opts TaskListOptions) (items []TaskView, total int, err error)
	// TaskMetrics returns ingested metric points (pure SQL read, spec
	// §8.1.4). afterTS > 0 returns only points newer than it — the ?after=
	// incremental pull for charts.
	TaskMetrics(ctx context.Context, taskID string, afterTS int64) ([]MetricPoint, error)
	// MetricKeys is the discovery half of the metrics dual-mode (spec §5.4:
	// GET /jobs/{id}/metrics without ?key=).
	MetricKeys(ctx context.Context, jobID string) ([]string, error)

	// ── RQ-44: log access through the OWNING target's filesystem ─────────
	//
	// Logs live where the task ran; these methods read them via that
	// target's rfs.FS (LocalFS and SSHFS share one lane implementation —
	// the lane doesn't know the difference). None of this needs
	// lifecycleMu: log reads produce no verdicts and touch no lifecycle
	// state. Establishing a follow counts as user activity (touchActivity).

	// TaskLogRead reads a page of lines. The ANCHOR is bytes (O(1) seek,
	// reconnect resume, heatmap jump — activity.tsv counts bytes); the
	// QUANTITY is lines (what the UI renders; alignment via
	// snap-to-line-start lives where the file is read).
	//
	// offset is a PURE byte position, io.Seek style — no sentinels:
	//   offset < 0     → error (tail view is TaskLogTail's job)
	//   0 ≤ offset     → every position is a valid question:
	//     offset < size  → a page of data
	//     offset ≥ size  → empty page + current Size, NO error. This is the
	//                      polling steady state ("caught up, nothing new")
	//                      and the rotation probe (caller sees Size < its
	//                      offset and restarts from 0).
	//
	// A log that doesn't exist yet (task pending) returns an empty page,
	// Size 0 and NO error. Implementations must bound worst-case bytes
	// internally (clamp line length and per-page byte budget — one
	// pathological 100MB line must not become one SFTP transfer), and must
	// guarantee PROGRESS: as long as bytes remain, NextOffset strictly
	// advances (bounded snap window — never rewind a continuation point
	// back across a >page-budget line). Arbitrary ranges are first-class:
	// vscode-style full reads just page through offsets.
	TaskLogRead(ctx context.Context, taskID string, offset int64, maxLines int) (*LogPage, error)

	// TaskLogTail returns the last maxLines lines — the entry point for
	// every first paint (CLI `runq logs`, dashboard log view, follow's
	// initial snapshot): the caller has no byte coordinates yet, and lines
	// can only be counted where the file lives. Resolution: count maxLines
	// '\n' backward from EOF (mind a trailing newline), then read forward —
	// i.e. resolve to a position, delegate to the positional read. The
	// returned page's Offset is the caller's anchor for paging UP; after
	// the first paint everything is positional TaskLogRead.
	// Pending task (no log yet) → empty page, Size 0, NO error.
	TaskLogTail(ctx context.Context, taskID string, maxLines int) (*LogPage, error)

	// TaskLogFollow follows the log from offset as the file grows,
	// delivering it as a LogPage ITERATOR — not a raw byte stream. Raw
	// bytes would force every consumer (SSE handler, CLI logs -f) to
	// re-implement line splitting, ANSI stripping and offset accounting;
	// pages inherit all of that from the positional read path for free.
	// Consumers are a bare loop: for { page := f.Next(ctx); emit(page) }.
	//
	// Implementation contract: ONE kept-open FS handle + adaptive polling
	// (~300ms while bytes flow, doubling to ~2s when quiet; a burst drains
	// at full speed since Next returns without sleeping while data
	// remains) — smooth to watch, with the resource shape of a human
	// running tail -f. Never ExecStream("tail -f"): that would pin one of
	// the target's few SSH session slots for as long as a browser tab
	// stays open. On rotation (current size < offset) silently restart
	// from 0 — the caller notices the page's Offset jumping backward and
	// resets its view. The caller MUST Close; ctx cancellation of a
	// blocked Next must also release it promptly.
	TaskLogFollow(ctx context.Context, taskID string, offset int64) (LogFollower, error)

	// JobLogSearch greps every task log of the job ON THE OWNING SIDE
	// (FS.Exec grep — results travel, files don't). Implementations may
	// cap the number of matches.
	JobLogSearch(ctx context.Context, jobID, query string) ([]LogMatch, error)
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
	Target        string // compute target name; empty = default_target
}
