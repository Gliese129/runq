package backend

import (
	"context"
	"testing"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/store"
)

// Codex RQ2-4 F3: during a config reconcile the default target can be
// momentarily unrouted — the removed loop unroutes the old default
// before the added loop installs its replacement, and an SSH lane build
// keeps that window open for seconds. Every default-routed reader must
// degrade (zero capabilities / retryable error), never panic: the first
// explicit target's creation DID apply, and a panicking /config turned
// that success into a 500.
func TestDefaultBackendReconcileGap(t *testing.T) {
	ctx := context.Background()
	st, serr := store.Open(":memory:")
	if serr != nil {
		t.Fatal(serr)
	}
	defer st.Close()
	lane := &UnavailableBackend{}
	m, err := NewMultiBackend(map[string]Backend{"local": lane}, st, "local")
	if err != nil {
		t.Fatal(err)
	}

	// Mid-reconcile shape 1: default unrouted, another lane already up →
	// readers fall back to the surviving lane.
	m.SetTarget("hpc", lane)
	m.RemoveTarget("local")
	_ = m.Capabilities() // must not panic
	if _, lerr := m.ListProjects(ctx); lerr == nil {
		t.Fatal("UnavailableBackend must surface its error, not succeed")
	}

	// Mid-reconcile shape 2: NO lane routable at all (old default retired,
	// replacement still building) — the exact window Codex reproduced.
	m.RemoveTarget("hpc")
	caps := m.Capabilities() // must not panic; zero value is the honest answer
	if caps.PauseResume || caps.Retry {
		t.Fatalf("gap capabilities must be zero, got %+v", caps)
	}
	if _, lerr := m.ListProjects(ctx); lerr == nil {
		t.Fatal("gap reads must return a retryable error, not succeed")
	}
	if _, derr := m.DryRun(ctx, job.JobConfig{}); derr == nil {
		t.Fatal("gap DryRun must return an error, not panic")
	}
}
