package backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gliese129/runq-lab/internal/job"
	"github.com/gliese129/runq-lab/internal/store"
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

func TestMultiBackendUnconfiguredState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	m, err := NewMultiBackend(map[string]Backend{}, st, "")
	if err != nil {
		t.Fatalf("empty MultiBackend must be valid: %v", err)
	}
	if got := m.DefaultTargetName(); got != "" {
		t.Fatalf("default target = %q, want empty", got)
	}
	if err := m.SetDefaultTarget(""); err != nil {
		t.Fatalf("setting empty default in empty state: %v", err)
	}
	jobs, err := m.ListJobs(ctx, "")
	if err != nil || len(jobs) != 0 {
		t.Fatalf("target-independent job list = %+v, err %v", jobs, err)
	}
	gpus, err := m.GPUStatus(ctx)
	if err != nil || len(gpus) != 0 {
		t.Fatalf("empty GPU aggregate = %+v, err %v", gpus, err)
	}

	_, err = m.DryRun(ctx, job.JobConfig{})
	if !errors.Is(err, ErrNoTargetConfigured) {
		t.Fatalf("DryRun error = %v, want ErrNoTargetConfigured", err)
	}
	if !strings.Contains(err.Error(), "runq target add") {
		t.Fatalf("DryRun error is not actionable: %v", err)
	}
	_, _, err = m.SubmitJob(ctx, job.JobConfig{}, SubmitOptions{})
	if !errors.Is(err, ErrNoTargetConfigured) {
		t.Fatalf("SubmitJob error = %v, want ErrNoTargetConfigured", err)
	}

	m.SetTarget("configured", NewUnavailableBackend(errors.New("unused")))
	if err := m.SetDefaultTarget(""); err == nil {
		t.Fatal("clearing the default while a target is active must fail")
	}
}
