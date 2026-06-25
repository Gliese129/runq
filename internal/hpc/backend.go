// Package hpc is the HPC backend: the second consumer of submitplan.Plan
// (alongside the daemon). It has no resident process — each CLI command opens
// the HPC store, does its work, and exits. Compute nodes only ever write files
// (status.json, metrics.jsonl); the DB is written exclusively here, on the
// login node.
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
package hpc

import (
	"context"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/store"
)

// Runner executes a shell command and returns its combined output. It is
// injectable so tests can stand in a fake cluster without sbatch/squeue.
type Runner func(ctx context.Context, command string) (string, error)

// shellRunner runs the command through `sh -c`, capturing stdout+stderr.
func shellRunner(ctx context.Context, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	return string(out), err
}

// Backend bundles the resolved config, the HPC store, and the command runner.
type Backend struct {
	Cfg        *hpcconfig.Config
	Store      *store.Store
	Run        Runner
	StorageCfg *config.GlobalConfig // nil-safe: nil = project_path mode

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

// New builds a Backend with the real shell runner.
func New(cfg *hpcconfig.Config, st *store.Store, storageCfg *config.GlobalConfig) *Backend {
	return &Backend{Cfg: cfg, Store: st, Run: shellRunner, StorageCfg: storageCfg}
}

// HPC-local filenames. These are backend artifacts, NOT part of the shared SDK
// file contract (which lives in internal/workspace) — the daemon never reads
// them, only the run.sh wrapper writes status.json.
const (
	runScriptName  = "run.sh"
	statusFileName = "status.json"
)

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
