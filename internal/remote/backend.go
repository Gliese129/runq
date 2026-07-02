// Package remote is the command-template-driven scheduler backend: it drives
// any scheduler reachable through shell commands (sbatch/squeue/scancel for
// Slurm, qsub/qstat/qdel for PBS/SGE — and, in the future, a remote `runq
// server` via its --json CLI). Formerly named "hpc"; renamed during the
// scheduler unification (RQ-46) because nothing here is scheduler-brand
// specific — this is "a scheduler you talk to by running commands", locally
// or over SSH (both via rfs.FS). Compute nodes only ever write files
// (status.json, metrics.jsonl); the DB is written exclusively client-side.
//
// The package reuses the shared kernel for everything that is "runq semantics":
// submitplan.Build (plan), workspace.Write (file contract), runqenv.Base (env
// contract), ingest.ReapOutputs (metrics projection). The HPC-specific glue is
// template rendering, run.sh generation, and DB reconciliation — the genuinely
// algorithmic bits (shell-safe interpolation, id extraction, status reconcile)
// live in reconcile.go (this package) and internal/utils (shell quoting).
//
// # Best-effort status (read this before reasoning about task state)
//
// Unlike the daemon, there is NO resident process driving state forward — no
// scheduler loop, no timer. Every status the DB holds is a BEST-EFFORT
// projection, recomputed only when a command calls EnsureFresh, of sources the
// user owns:
//
//   - the wrapper's <task_dir>/status.json (written by run.sh / the SDK), and
//   - an OPTIONAL scheduler probe (status_template, + status_parser).
//
// Consequences, by design:
//
//   - State only advances when a command calls EnsureFresh on that job. Between
//     calls the DB is frozen; a job can finish on the cluster and still read
//     "running" until the next reconcile. That is expected.
//   - The honest default for a submitted-but-not-yet-terminal task is "(maybe)
//     running". runq does NOT own liveness truth — whether running/done is
//     accurate depends on the user's shell integration (run.sh writing
//     status.json, and/or a correct status_template/status_parser). runq only
//     faithfully reflects those files + the optional probe.
//   - Precise liveness (scheduler probe, gone/zombie detection) is an OPTIONAL
//     accuracy enhancement the user opts into via status_template; the core path
//     (status.json only) is deliberately coarse.
//   - The DB never records a fact runq does not know: a task only becomes
//     "killed" after a cancel actually succeeds (see Kill), never just because a
//     kill was requested.
package remote

import (
	"context"
	"fmt"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
)

// runner executes a shell command and returns its combined output. Internal
// function type used by memoRunner for caching probe results within a
// reconcile pass. Derived from FS.Exec by Backend.shellRun.
type runner func(ctx context.Context, command string) (string, error)

// TaskFinisher is the slice of scheduler.Scheduler that reconcile needs to
// drive tasks to terminal states through the shared lifecycle (retry policy,
// slot release, queue bookkeeping). Satisfied by *scheduler.Scheduler.
type TaskFinisher interface {
	FinishTask(t *scheduler.Task, status scheduler.TaskStatus, extra map[string]any)
}

// Backend bundles the resolved config, the HPC store, and the filesystem.
// FS handles both file I/O and command execution — LocalFS for same-machine
// operation, SSHFS for remote clusters. Commands go through shellRun which
// wraps FS.Exec("sh", "-c", ...).
type Backend struct {
	Cfg        *config.TargetConfig
	Store      *store.Store
	FS         rfs.FS
	StorageCfg *config.GlobalConfig // nil-safe: nil = project_path mode

	// Finisher, when set (daemon assembly), routes terminal state transitions
	// through the target's scheduler instance — the single lifecycle funnel
	// (retry, slot release, queue). nil = legacy direct-DB-write mode, used
	// by tests and any path not yet running under a per-target scheduler.
	Finisher TaskFinisher

	// Per-job TTL cache (see refresh.go): tracks when each job's scheduler
	// was last probed, so EnsureFresh can skip the probe (not the local
	// reconcile) within the caller's TTL window.
	probeMu   sync.Mutex
	lastProbe map[string]time.Time

	// Batch probe TTL: when status_list_template is configured,
	// EnsureAllFresh skips the batch query if the last batch probe
	// completed within the TTL window.
	lastBatchProbe time.Time
}

// New builds a Backend with a local filesystem (same-machine operation).
func New(cfg *config.TargetConfig, st *store.Store, storageCfg *config.GlobalConfig) *Backend {
	return &Backend{Cfg: cfg, Store: st, FS: rfs.NewLocalFS(), StorageCfg: storageCfg}
}

// NewWithFS builds a Backend with an explicit filesystem — used by SSHBackend
// to inject an rfs.SSHFS for remote cluster operation.
func NewWithFS(cfg *config.TargetConfig, st *store.Store, storageCfg *config.GlobalConfig, fsys rfs.FS) *Backend {
	return &Backend{Cfg: cfg, Store: st, FS: fsys, StorageCfg: storageCfg}
}

// shellRun executes a shell command through the FS layer, returning combined
// stdout+stderr and any error (including non-zero exit). This is the FS-backed
// replacement for the old Runner/shellRunner pattern.
func (b *Backend) shellRun(ctx context.Context, command string) (string, error) {
	stdout, stderr, code, err := b.FS.Exec(ctx, "sh", "-c", command)
	combined := string(stdout) + string(stderr)
	if err != nil {
		return combined, err
	}
	if code != 0 {
		return combined, fmt.Errorf("exit status %d", code)
	}
	return combined, nil
}

// Remote-lane filenames. These are backend artifacts, NOT part of the shared
// SDK file contract (which lives in internal/workspace) — only the run.sh
// wrapper writes status.json; the daemon reads them via rfs.FS.
const (
	runScriptName  = "run.sh"
	statusFileName = "status.json"
	doneDirName    = ".runq-done"
)

// doneDir returns the target-level done-marker directory, or "" when the
// target has no workspace root configured (marker detection disabled; the
// probe path still works). Shared across projects on purpose: one readdir
// covers every in-flight task on this target.
func (b *Backend) doneDir() string {
	if b.Cfg == nil || b.Cfg.Workspace == "" {
		return ""
	}
	return path.Join(b.Cfg.Workspace, doneDirName)
}

// rowToTask builds the minimal scheduler.Task that FinishTask needs from a
// store row. Only lifecycle-relevant fields are populated (retry decision,
// queue/job bookkeeping) — this is a projection, not a full rehydration.
func rowToTask(tk store.TaskRow) *scheduler.Task {
	return &scheduler.Task{
		ID:         tk.ID,
		JobID:      tk.JobID,
		RetryCount: tk.RetryCount,
		MaxRetry:   tk.MaxRetry,
		TaskDir:    tk.TaskDir,
		LogPath:    tk.LogPath,
	}
}

// terminalStatuses are the states a task never leaves.
func isTerminal(status string) bool {
	switch status {
	case "success", "failed", "killed":
		return true
	}
	return false
}

// sortedKeys returns map keys in deterministic order (stable run.sh output).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// refreshJobStatus derives the job's aggregate status from its tasks, mirroring
// the daemon's service.refreshJobStatus so daemon and HPC report jobs the same
// way.
func (b *Backend) refreshJobStatus(ctx context.Context, jobID string) error {
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	var running, pending, done int
	for _, t := range tasks {
		switch {
		case t.Status == "running":
			running++
		case t.Status == "pending":
			pending++
		case isTerminal(t.Status):
			done++
		}
	}
	started := running+done > 0
	ended := pending+running == 0

	status := "pending"
	switch {
	case ended:
		status = "done"
	case started:
		status = "running"
	}
	return b.Store.UpdateJobStatus(ctx, jobID, status)
}

// nowUnix is a seam for tests; production uses the wall clock.
var nowUnix = func() int64 { return time.Now().Unix() }
