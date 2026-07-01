package backend

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/hpc"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
)

// SSHBackend implements Backend for a remote HPC cluster accessed via SSH.
// Each SSHBackend holds its own rfs.SSHFS (persistent SSH connection) and
// per-target scheduler templates — no shared hpc: config section needed.
//
// Reconcile strategy (push from run.sh + poll):
//   - Background marker polling: daemon periodically ls .done/<task_id>
//     to detect completed tasks (lightweight, 30-60s interval).
//   - On-demand qstat: user clicks Refresh in the dashboard, triggering
//     a full scheduler probe for the job.
//   - status.json: written by run.sh on compute nodes, read by daemon via SSH.
type SSHBackend struct {
	storeQueries // embeds store, reg, and shared project/clean/thaw/dryrun methods
	backend      *hpc.Backend
	sshFS        *rfs.SSHFS // held for Close()
}

// SSHBackendConfig bundles everything needed to build an SSHBackend.
type SSHBackendConfig struct {
	Target    config.TargetConfig
	Store     *store.Store
	GlobalCfg *config.GlobalConfig
}

// NewSSHBackend creates a Backend for a remote HPC target. The SSH
// connection is lazy — no dial happens until the first operation.
//
// The caller must call Close() on shutdown to release the SSH connection.
func NewSSHBackend(cfg SSHBackendConfig) (*SSHBackend, error) {
	t := cfg.Target
	if t.SSH == nil {
		return nil, fmt.Errorf("target %q: ssh section is required for scheduler targets", t.Name)
	}
	if t.SubmitTemplate == "" {
		return nil, fmt.Errorf("target %q: submit_template is required", t.Name)
	}

	// Build SSH config from target.
	host := t.SSH.Host
	if t.SSH.Port > 0 {
		host = fmt.Sprintf("%s:%d", t.SSH.Host, t.SSH.Port)
	}

	auth, err := resolveSSHAuth(t.SSH)
	if err != nil {
		return nil, fmt.Errorf("target %q: ssh auth: %w", t.Name, err)
	}

	sshCfg := rfs.SSHConfig{
		Host:       host,
		User:       t.SSH.User,
		AuthMethod: auth,
	}

	sshFS := rfs.NewSSHFS(sshCfg)
	hpcBe := hpc.NewWithFS(&cfg.Target, cfg.Store, cfg.GlobalCfg, sshFS)

	return &SSHBackend{
		storeQueries: storeQueries{
			store: cfg.Store,
			reg:   project.NewRegistry(cfg.Store.DB()),
		},
		backend: hpcBe,
		sshFS:   sshFS,
	}, nil
}

// Close releases the SSH connection. Must be called on daemon shutdown.
func (b *SSHBackend) Close() error {
	return b.sshFS.Close()
}

// ── Capabilities ──────────────────────────────────────────────────────────

func (b *SSHBackend) Capabilities() Capabilities {
	return Capabilities{
		GPUMap:        false,  // no node-local GPU visibility
		PauseResume:   false,  // cluster queues have no runq-level pause
		LiveLog:       true,   // logs readable via SSH
		Retry:         false,  // no resident process to re-submit
		StateModel:    "poll", // best-effort projection; staleness surfaced
		KillAsync:     true,   // qdel/scancel forwarded
		SubmitPreview: true,   // zero-disk dry-run via submit code path
	}
}

// ── Reconcile ─────────────────────────────────────────────────────────────

func (b *SSHBackend) RefreshJob(ctx context.Context, jobID string) error {
	return b.backend.EnsureFresh(ctx, jobID, 0)
}

// ReconcileAll runs a full reconcile pass over all active jobs. Called by
// the dashboard's background ticker — never by the list endpoint itself.
func (b *SSHBackend) ReconcileAll(ctx context.Context) error {
	return b.backend.EnsureAllFresh(ctx, DefaultReadTTL)
}

// ── Job operations ────────────────────────────────────────────────────────

func (b *SSHBackend) ListJobs(ctx context.Context, projectScope string) ([]JobSummary, error) {
	target := b.backend.Cfg.Name
	jobs, err := b.store.ListJobsVisible(ctx, projectScope, target)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Status != "done" {
			_ = b.backend.EnsureFresh(ctx, j.ID, DefaultReadTTL)
		}
	}
	// Re-query after reconcile.
	jobs, err = b.store.ListJobsVisible(ctx, projectScope, target)
	if err != nil {
		return nil, err
	}
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}
	allTasks, err := b.store.ListTasksForJobs(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	byJob := make(map[string][]store.TaskRow, len(jobs))
	for _, t := range allTasks {
		byJob[t.JobID] = append(byJob[t.JobID], t)
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, BuildJobSummary(j, byJob[j.ID]))
	}
	return out, nil
}

func (b *SSHBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	j, err := b.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	detail := BuildJobDetail(*j, tasks)
	if cfg, err := b.reg.Get(ctx, j.ProjectName); err == nil && cfg.Wandb != nil {
		detail.Wandb = &WandbInfo{
			Entity:  cfg.Wandb.Entity,
			Project: cfg.Wandb.Project,
			BaseURL: WandbBaseURL(cfg.Wandb.Entity, cfg.Wandb.Project),
		}
	}
	return &detail, nil
}

func (b *SSHBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	if err := b.backend.EnsureFresh(ctx, jobID, DefaultReadTTL); err != nil {
		return nil, err
	}
	tasks, err := b.store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return BuildCompareRows(tasks, key, desc), nil
}

func (b *SSHBackend) GPUStatus(_ context.Context) ([]GPUSlot, error) {
	return []GPUSlot{}, nil
}

// ── Task operations ───────────────────────────────────────────────────────

func (b *SSHBackend) GetTask(ctx context.Context, taskID string) (*TaskView, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	task = b.reconcileTask(ctx, taskID, task)
	view := BuildTaskView(*task)
	return &view, nil
}

func (b *SSHBackend) TaskMetrics(ctx context.Context, taskID string) ([]MetricPoint, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	task = b.reconcileTask(ctx, taskID, task)
	return ReadMetricPoints(task.TaskDir), nil
}

func (b *SSHBackend) reconcileTask(ctx context.Context, taskID string, fallback *store.TaskRow) *store.TaskRow {
	if err := b.backend.EnsureFresh(ctx, fallback.JobID, DefaultReadTTL); err != nil {
		return fallback
	}
	if fresh, err := b.store.GetTask(ctx, taskID); err == nil && fresh != nil {
		return fresh
	}
	return fallback
}

func (b *SSHBackend) KillTask(ctx context.Context, taskID string) error {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("task %q: %w", taskID, ErrNotFound)
	}
	if err := b.backend.EnsureFresh(ctx, task.JobID, 0); err != nil {
		return fmt.Errorf("reconcile before kill: %w", err)
	}
	task, err = b.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil || task.Status == "success" || task.Status == "failed" || task.Status == "killed" {
		return nil
	}
	_, err = b.backend.Kill(ctx, taskID)
	return err
}

func (b *SSHBackend) RetryTask(_ context.Context, _ string) error {
	return fmt.Errorf("retry task in ssh mode: %w", ErrNotSupported)
}

func (b *SSHBackend) KillJob(ctx context.Context, jobID string) error {
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		return fmt.Errorf("reconcile before kill: %w", err)
	}
	_, err := b.backend.Kill(ctx, jobID)
	return err
}

func (b *SSHBackend) PauseJob(_ context.Context, _ string) error {
	return fmt.Errorf("pause job in ssh mode: %w", ErrNotSupported)
}

func (b *SSHBackend) ResumeJob(_ context.Context, _ string) error {
	return fmt.Errorf("resume job in ssh mode: %w", ErrNotSupported)
}

// ── Submit ────────────────────────────────────────────────────────────────

func (b *SSHBackend) SubmitJob(ctx context.Context, cfg job.JobConfig, opts SubmitOptions) (string, int, error) {
	proj, err := b.reg.Get(ctx, cfg.Project)
	if err != nil {
		return "", 0, fmt.Errorf("project %q: %w", cfg.Project, err)
	}
	return b.backend.Submit(ctx, cfg, proj, hpc.SubmitOpts{SkipPreflight: opts.SkipPreflight})
}

func (b *SSHBackend) PreviewSubmit(ctx context.Context, cfg job.JobConfig, skipPreflight bool) (string, error) {
	proj, err := b.reg.Get(ctx, cfg.Project)
	if err != nil {
		return "", fmt.Errorf("project %q: %w", cfg.Project, err)
	}
	return b.backend.Preview(ctx, cfg, proj, skipPreflight)
}

func (b *SSHBackend) ResolveNote(ctx context.Context, cfg job.JobConfig) (string, error) {
	rows, err := b.store.ListJobs(ctx, cfg.Project, "")
	if err != nil {
		return "", err
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		notes = append(notes, r.Note)
	}
	return job.RenderNote(&cfg, job.NoteContext{
		Project: cfg.Project, Now: time.Now(), ExistingNotes: notes,
	})
}

// ── Archive ───────────────────────────────────────────────────────────────

func (b *SSHBackend) ListArchivedJobs(ctx context.Context) ([]JobSummary, error) {
	jobs, err := b.store.ListJobsArchived(ctx, "", b.backend.Cfg.Name)
	if err != nil {
		return nil, err
	}
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}
	allTasks, err := b.store.ListTasksForJobs(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	byJob := make(map[string][]store.TaskRow, len(jobs))
	for _, t := range allTasks {
		byJob[t.JobID] = append(byJob[t.JobID], t)
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, BuildJobSummary(j, byJob[j.ID]))
	}
	return out, nil
}

func (b *SSHBackend) ArchiveJob(ctx context.Context, jobID string) error {
	if err := b.backend.EnsureFresh(ctx, jobID, 0); err != nil {
		return fmt.Errorf("reconcile before archive: %w", err)
	}
	return b.store.ArchiveJob(ctx, jobID)
}

func (b *SSHBackend) UnarchiveJob(ctx context.Context, jobID string) error {
	return b.store.UnarchiveJob(ctx, jobID)
}

// ── Project operations ────────────────────────────────────────────────────

func (b *SSHBackend) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	configs, err := b.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	return b.configsToSummaries(ctx, configs)
}

// ── SSH auth helper ───────────────────────────────────────────────────────

// resolveSSHAuth builds an ssh.AuthMethod from the target's SSH config.
// If Key is set, reads the private key file. Otherwise falls back to
// the SSH agent (SSH_AUTH_SOCK).
func resolveSSHAuth(cfg *config.SSHTargetConfig) (ssh.AuthMethod, error) {
	if cfg.Key != "" {
		keyBytes, err := os.ReadFile(cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("read key %q: %w", cfg.Key, err)
		}
		signer, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse key %q: %w", cfg.Key, err)
		}
		return ssh.PublicKeys(signer), nil
	}

	// Fall back to ssh-agent.
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("no key file and SSH_AUTH_SOCK not set")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("connect to ssh-agent: %w", err)
	}
	// Note: conn is intentionally not closed here — the agent client holds
	// it for the lifetime of the SSH connection. The OS reclaims it on exit.
	agentClient := agent.NewClient(conn)
	return ssh.PublicKeysCallback(agentClient.Signers), nil
}

// compile-time interface check
var _ Backend = (*SSHBackend)(nil)
