package backend

import (
	"context"
	"time"
)

// DefaultReadTTL is the standard freshness window for read operations.
// Within this window a second call to EnsureFresh is a no-op.
const DefaultReadTTL = 15 * time.Second

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
