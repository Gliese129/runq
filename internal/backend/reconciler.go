package backend

import (
	"context"
	"time"
)

// DefaultReadTTL is the minimum interval between scheduler probes (qstat,
// squeue, etc.) for a given job. Within this window EnsureFresh skips the
// probe — only cheap local reads (status.json, metrics.jsonl) run.
//
// 30 s is chosen as the floor for HPC scheduler queries:
//   - Scheduler daemons (pbs_server, slurmctld, sge_qmaster) are shared
//     infrastructure. Polling faster than ~30 s risks throttling or ban,
//     especially on busy clusters.
//   - The dashboard's reconcileLoop ticks at this same interval, so the
//     background batch probe (one `qstat -u $USER`) pre-fills every job's
//     TTL cache. Inline reads (ListJobs, GetJob, …) then only do local
//     file reads — fast, no scheduler traffic.
//   - CLI callers (no reconcileLoop) still obey the TTL: `watch -n 1
//     runq ps` fires a probe on the first call, then only local reads
//     for the next 30 s. The user gets responsive file-level updates
//     without hammering the scheduler.
const DefaultReadTTL = 30 * time.Second

// Reconciler ensures backend data is fresh before reads and writes.
//
// The contract: every Backend method that reads or mutates job/task state
// calls EnsureFresh (or EnsureAllFresh) BEFORE touching the store. This
// makes the Backend interface's semantic guarantee uniform: data is fresh,
// operations are real — regardless of whether the backend is push-model
// (daemon) or poll-model (HPC).
//
//   - Push-model (daemon): NoopReconciler — data is always current.
//   - Poll-model (HPC): HPCReconciler — reconciles from status.json +
//     optional scheduler probe, with a per-job TTL cache so the login
//     node isn't hammered.
type Reconciler interface {
	// EnsureFresh ensures jobID's data is no older than ttl. ttl=0 forces
	// a full reconcile (scheduler probe included).
	EnsureFresh(ctx context.Context, jobID string, ttl time.Duration) error
	// EnsureAllFresh reconciles all active (non-done) jobs within the TTL.
	EnsureAllFresh(ctx context.Context, ttl time.Duration) error
}

// NoopReconciler satisfies Reconciler for push-model backends where data
// is always current (daemon mode).
type NoopReconciler struct{}

func (NoopReconciler) EnsureFresh(context.Context, string, time.Duration) error { return nil }
func (NoopReconciler) EnsureAllFresh(context.Context, time.Duration) error      { return nil }
