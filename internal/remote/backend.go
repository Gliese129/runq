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
// submitplan.Build (plan), workspace.Write (file contract), workspace.BaseEnv (env
// contract), ingest.ReapIncremental (metrics projection). The HPC-specific glue is
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
	"strings"
	"sync"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/scheduler"
	"github.com/gliese129/runq-lab/internal/store"
)

// runner executes a shell command and returns (combined output, exit code,
// transport error). The three-way split is load-bearing for probe semantics:
// a TRANSPORT error means the command never ran — no fact was learned and it
// must never be interpreted as a scheduler verdict (a blip parsed as "gone"
// would fail healthy tasks). An EXIT code is the scheduler talking.
type runner func(ctx context.Context, command string) (string, int, error)

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
	Cfg *config.TargetConfig
	// Scope is this lane's generation-ownership predicate (RQ-75): EVERY
	// lifecycle query (probe candidates, marker reconcile, heartbeat,
	// orphan detection) filters by it, so active and retiring lanes never
	// touch each other's tasks. nil = unscoped (legacy/runqd paths).
	Scope      *store.LaneScope
	Store      *store.Store
	FS         rfs.FS
	StorageCfg *config.GlobalConfig // nil-safe: nil = project_path mode

	// Finisher, when set (daemon assembly), routes terminal state transitions
	// through the target's scheduler instance — the single lifecycle funnel
	// (retry, slot release, queue). nil = legacy direct-DB-write mode, used
	// by tests and any path not yet running under a per-target scheduler.
	Finisher TaskFinisher

	// HOME restoration (RQ-76 ①): the login node's absolute $HOME,
	// resolved ONCE per lane and baked into run.sh as `export HOME=...`
	// so tilde expansion works on compute nodes even under
	// --export=NONE. A lane lives exactly one target generation, so a
	// config edit (new lane) naturally re-resolves.
	homeOnce sync.Once
	homeDir  string
	homeErr  error

	// Per-job TTL cache (see refresh.go): tracks when each job's scheduler
	// was last probed, so EnsureFresh can skip the probe (not the local
	// reconcile) within the caller's TTL window.
	probeMu   sync.Mutex
	lastProbe map[string]time.Time

	// Per-job local-scan cache (guarded by probeMu, see refresh.go): when
	// the last complete file-scan pass for a job finished, so probe-free
	// EnsureFresh calls within localScanTTL can skip the scan entirely —
	// back-to-back dashboard reads (task GET + job GET) coalesce instead
	// of serializing on lifecycleMu and repeating the same SFTP reads.
	lastLocalScan map[string]time.Time

	// Batch probe TTL: when status_list_template is configured,
	// SchedulerProbe skips the batch query if the last batch probe
	// completed within the TTL window.
	lastBatchProbe time.Time

	// orphanStrikes counts consecutive missing-dir observations per task
	// (guarded by probeMu). See DetectOrphans — two strikes mark, presence
	// clears. In-memory: restart resets, erring on the safe side.
	orphanStrikes map[string]int

	// lastContact is the PASSIVE reachability record (spec §5.1 /health,
	// D6): outcome of the most recent transport-touching operation. Updated
	// as a side effect of work that was happening anyway — never probed.
	contactMu  sync.Mutex
	contactAt  time.Time
	contactErr string // "" = last contact succeeded
	// Write-through debounce for the sync_state 'contact' row (L4): the
	// record persists so /health and doctor can say "checked 2h ago"
	// across daemon restarts instead of the lie "no contact yet".
	contactWroteAt   time.Time
	contactWroteErr  string
	contactEverWrote bool

	// lifecycleMu serializes every verdict-producing pass on this lane —
	// EnsureFresh / SchedulerProbe / HeartbeatProbe / ScanDoneMarkers / Kill — plus manual
	// retry (via WithLifecycleLock). THE staleness invariant of the remote
	// lane rests on it: a requeue only ever happens inside a verdict delivery.
	// Different target generations have different lane locks, so Store's
	// durable attempt CAS and per-task attempt-file lock fence cross-generation
	// reads against a retry that starts attempt N+1.
	//
	// INVARIANT: any new code path that reads task state and may call
	// persistDecision/FinishTask, or that resets a task for relaunch, MUST
	// hold this lock. Per-lane lock: no cross-target contention; these
	// paths are SSH-bound and effectively serial anyway.
	lifecycleMu sync.Mutex
}

// WithLifecycleLock runs fn under this lane's lifecycle lock. Exposed for
// the one non-verdict identity mutation outside this package: user-initiated
// retry (SSHBackend.RetryTask), which must not interleave with an in-flight
// verdict delivery.
func (b *Backend) WithLifecycleLock(fn func() error) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	return fn()
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

// shellRunClassified executes a shell command through the FS layer and
// preserves the transport-vs-verdict distinction (see runner).
func (b *Backend) shellRunClassified(ctx context.Context, command string) (string, int, error) {
	stdout, stderr, code, err := b.FS.Exec(ctx, "sh", "-c", command)
	b.recordContact(err) // transport outcome only: exit codes are scheduler verdicts, not reachability
	return string(stdout) + string(stderr), code, err
}

// recordContact updates the passive reachability record. ok = the
// transport worked (a non-zero exit code still proves the target is
// reachable). Call it from paths that touch the transport anyway.
func (b *Backend) recordContact(transportErr error) {
	b.contactMu.Lock()
	defer b.contactMu.Unlock()
	b.contactAt = time.Now()
	if transportErr != nil {
		b.contactErr = transportErr.Error()
	} else {
		b.contactErr = ""
	}
	// Persist (debounced): state flips write immediately, steady state at
	// most once a minute — recordContact fires per SSH command, and a
	// contact timestamp precise to the minute is precise enough.
	flip := b.contactErr != b.contactWroteErr
	if b.Store != nil && (!b.contactEverWrote || flip || time.Since(b.contactWroteAt) > time.Minute) {
		b.contactWroteAt = time.Now()
		b.contactWroteErr = b.contactErr
		b.contactEverWrote = true
		_ = b.Store.RecordSyncOutcome(context.Background(), b.Cfg.Name, "contact", transportErr)
	}
}

// RecordContactOK records a daemon-observed successful contact with this
// target from OUTSIDE the normal exec paths (RQ-74): the remote CLI forward
// session coming up after `runq connect`, or any other transport-level proof
// of reachability. Doctor's "no contact yet" clears the moment the daemon
// has such proof, instead of waiting for the next scheduled sensor pass.
func (b *Backend) RecordContactOK() { b.recordContact(nil) }

// LastContact returns the passive reachability snapshot for /health:
// zero time = no contact since boot.
func (b *Backend) LastContact() (at time.Time, ok bool, lastErr string) {
	b.contactMu.Lock()
	at, ok, lastErr = b.contactAt, b.contactErr == "", b.contactErr
	b.contactMu.Unlock()
	if !at.IsZero() || b.Store == nil {
		return at, ok, lastErr
	}
	// No contact THIS process — fall back to the persisted row: the fact
	// "reached it 2h ago" belongs to the target, not to the process that
	// observed it.
	if row, err := b.Store.GetSyncState(context.Background(), b.Cfg.Name, "contact"); err == nil && row != nil && row.LastAttempt > 0 {
		return time.Unix(row.LastAttempt, 0), row.LastError == "", row.LastError
	}
	return at, ok, lastErr
}

// shellRun is the collapsed convenience form for callers that treat any
// failure the same way (kill, marker cleanup, legacy submit): non-zero exit
// becomes an error.
func (b *Backend) shellRun(ctx context.Context, command string) (string, error) {
	out, code, err := b.shellRunClassified(ctx, command)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("exit status %d", code)
	}
	return out, nil
}

// Remote-lane filenames. These are backend artifacts, NOT part of the shared
// SDK file contract (which lives in internal/workspace) — only the run.sh
// wrapper writes status.json; the daemon reads them via rfs.FS.
const (
	runScriptName  = "run.sh"
	statusFileName = "status.json"
	doneDirName    = ".runq-done"
)

// doneDir returns the target-level done-marker directory: explicit done_dir
// wins, then <workspace>/.runq-done, else "" (marker detection disabled; the
// probe path still works). Shared across projects on purpose: one readdir
// covers every in-flight task on this target.
func (b *Backend) doneDir() string {
	if b.Cfg == nil {
		return ""
	}
	if b.Cfg.DoneDir != "" {
		return b.Cfg.DoneDir
	}
	if b.Cfg.Workspace == "" {
		return ""
	}
	return path.Join(b.Cfg.Workspace, doneDirName)
}

// ResetWrapperState clears a previous attempt's status.json and done marker
// so a (re)launch starts from a blank slate — a stale terminal status.json
// would be re-reconciled into an instant failure while the new cluster job
// is still queued. Called by Launcher.Launch before every submission and by
// user-initiated retry.
func (b *Backend) ResetWrapperState(ctx context.Context, taskDir, taskID string) error {
	statusFile := path.Join(taskDir, statusFileName)
	if err := b.FS.WriteFile(statusFile, []byte("{}\n"), 0o644); err != nil {
		return fmt.Errorf("reset %s: %w", statusFile, err)
	}
	if dir := b.doneDir(); dir != "" {
		marker := path.Join(dir, taskID)
		_, stderr, exitCode, err := b.FS.Exec(ctx, "rm", "-f", marker)
		if err != nil {
			return fmt.Errorf("reset done marker %s: %w", marker, err)
		}
		if exitCode != 0 {
			return fmt.Errorf("reset done marker %s: rm exited %d: %s", marker, exitCode, strings.TrimSpace(string(stderr)))
		}
	}
	return nil
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
		// Informational only (restore/display); staleness is handled by
		// lifecycleMu serialization, not by id comparison.
		ExternalID: tk.ExternalID,
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
	// Deliberately UNscoped (review round 3): the job aggregate must see
	// ALL generations' tasks — a lane judging terminality from only its
	// own subset would close a job whose other-generation tasks still run.
	tasks, err := b.Store.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	status, err := store.ProjectJobStatus(tasks)
	if err != nil {
		return err
	}
	return b.Store.UpdateJobStatus(ctx, jobID, status)
}

// nowUnix is a seam for tests; production uses the wall clock.
var nowUnix = func() int64 { return time.Now().Unix() }
