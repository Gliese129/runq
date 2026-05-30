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
// live in internal/hpccore.
package hpc

import (
	"context"
	"os/exec"
	"sort"
	"time"

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
	Cfg   *hpcconfig.Config
	Store *store.Store
	Run   Runner
}

// New builds a Backend with the real shell runner.
func New(cfg *hpcconfig.Config, st *store.Store) *Backend {
	return &Backend{Cfg: cfg, Store: st, Run: shellRunner}
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
