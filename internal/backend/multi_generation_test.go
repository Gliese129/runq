package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/store"
)

type generationTestLane struct {
	*UnavailableBackend
	refreshErr      error
	refreshErrByJob map[string]error
	recordedSyncErr error
	refreshCalls    int
	pauseCalls      int
	resumeCalls     int
	reconcileCalls  int
	listCalls       int
	listedJobs      []JobSummary
	forceCalls      int
	forceReceipt    *RefreshReceipt
	forceErr        error
	syncAt          int64
	syncStale       bool
}

func newGenerationTestLane() *generationTestLane {
	return &generationTestLane{UnavailableBackend: NewUnavailableBackend(nil)}
}

func (l *generationTestLane) RefreshJob(_ context.Context, jobID string) error {
	l.refreshCalls++
	if l.refreshErrByJob != nil {
		return l.refreshErrByJob[jobID]
	}
	return l.refreshErr
}

func (l *generationTestLane) recordTasksSync(err error) {
	l.recordedSyncErr = err
	l.syncStale = err != nil
}

func (l *generationTestLane) PauseJob(context.Context, string) error {
	l.pauseCalls++
	return nil
}

func (l *generationTestLane) ResumeJob(context.Context, string) error {
	l.resumeCalls++
	return nil
}

func (l *generationTestLane) ReconcileAll(context.Context) error {
	l.reconcileCalls++
	return nil
}

func (l *generationTestLane) ListJobs(context.Context, string) ([]JobSummary, error) {
	l.listCalls++
	return l.listedJobs, nil
}

func (l *generationTestLane) ForceRefresh(context.Context) (*RefreshReceipt, error) {
	l.forceCalls++
	return l.forceReceipt, l.forceErr
}

func (l *generationTestLane) SyncInfo(context.Context) (int64, bool) {
	return l.syncAt, l.syncStale
}

func seedGenerationJob(t *testing.T, st *store.Store, jobID string, generations ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: jobID, ProjectName: "p", Status: "running", TotalTasks: len(generations),
		CreatedAt: time.Now(), Target: "hpc",
	}); err != nil {
		t.Fatal(err)
	}
	for i, generation := range generations {
		if err := st.InsertTask(ctx, &store.TaskRow{
			ID: jobID + "-t" + string(rune('a'+i)), JobID: jobID, ProjectName: "p",
			Command: "true", ParamsJSON: "{}", Status: "running",
			Target: "hpc", TargetGeneration: generation, ExternalID: "external",
			EnqueuedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func newGenerationMulti(t *testing.T, st *store.Store, active *generationTestLane) *MultiBackend {
	t.Helper()
	m, err := NewMultiBackend(map[string]Backend{"hpc": active}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPauseResumeFanOutToEveryOwningGeneration(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedGenerationJob(t, st, "job-control", "active", "old")

	active := newGenerationTestLane()
	old := newGenerationTestLane()
	unused := newGenerationTestLane()
	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old", old)
	m.SetRetiringLane("hpc", "unused", unused)

	ctx := context.Background()
	if err := m.PauseJob(ctx, "job-control"); err != nil {
		t.Fatalf("pause job: %v", err)
	}
	if err := m.ResumeJob(ctx, "job-control"); err != nil {
		t.Fatalf("resume job: %v", err)
	}
	if active.pauseCalls != 1 || old.pauseCalls != 1 || unused.pauseCalls != 0 {
		t.Fatalf("pause calls active/old/unused = %d/%d/%d, want 1/1/0",
			active.pauseCalls, old.pauseCalls, unused.pauseCalls)
	}
	if active.resumeCalls != 1 || old.resumeCalls != 1 || unused.resumeCalls != 0 {
		t.Fatalf("resume calls active/old/unused = %d/%d/%d, want 1/1/0",
			active.resumeCalls, old.resumeCalls, unused.resumeCalls)
	}
}

func TestRefreshJobDoesNotLetEmptyActiveScopeHideRetiringFailure(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedGenerationJob(t, st, "job-retired-only", "old")

	active := newGenerationTestLane()
	old := newGenerationTestLane()
	retiringErr := errors.New("retiring refresh failed")
	old.refreshErr = retiringErr
	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old", old)

	err = m.RefreshJob(context.Background(), "job-retired-only")
	if !errors.Is(err, retiringErr) {
		t.Fatalf("RefreshJob error = %v, want retiring failure", err)
	}
	if active.refreshCalls != 0 || old.refreshCalls != 1 {
		t.Fatalf("refresh calls active/old = %d/%d, want 0/1", active.refreshCalls, old.refreshCalls)
	}
}

func TestGenerationCompleteRefreshPrecedesJobScopedStoreReads(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedGenerationJob(t, st, "job-read", "active", "old")

	active := newGenerationTestLane()
	old := newGenerationTestLane()
	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old", old)
	ctx := context.Background()

	tests := []struct {
		name    string
		read    func() error
		wantErr bool
	}{
		{name: "GetJob", read: func() error { _, err := m.GetJob(ctx, "job-read"); return err }},
		{name: "CompareMetrics", read: func() error { _, err := m.CompareMetrics(ctx, "job-read", "loss", false); return err }},
		{name: "JobResults", read: func() error { _, err := m.JobResults(ctx, "job-read"); return err }},
		{name: "ArchiveJob", read: func() error { return m.ArchiveJob(ctx, "job-read") }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			active.refreshCalls = 0
			old.refreshCalls = 0
			err := tt.read()
			if err != nil && !tt.wantErr {
				t.Fatal(err)
			}
			if err == nil && tt.wantErr {
				t.Fatal("expected store validation error")
			}
			if active.refreshCalls != 1 || old.refreshCalls != 1 {
				t.Fatalf("refresh calls active/old = %d/%d, want 1/1", active.refreshCalls, old.refreshCalls)
			}
		})
	}
}

func TestJobListRefreshesEveryOwningGenerationBeforeReread(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedGenerationJob(t, st, "job-list", "active", "old")

	active := newGenerationTestLane()
	old := newGenerationTestLane()
	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old", old)

	jobs, err := m.ListJobsForTarget(context.Background(), "hpc", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-list" {
		t.Fatalf("jobs = %+v, want job-list", jobs)
	}
	if active.refreshCalls != 1 || old.refreshCalls != 1 {
		t.Fatalf("refresh calls active/old = %d/%d, want 1/1", active.refreshCalls, old.refreshCalls)
	}
	if active.listCalls != 0 || old.listCalls != 0 {
		t.Fatalf("lane list calls active/old = %d/%d, want durable-store rendering", active.listCalls, old.listCalls)
	}
}

func TestJobListAggregatesEveryObservationBeforePublishingFreshness(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedGenerationJob(t, st, "job-refresh-fails", "active")
	seedGenerationJob(t, st, "job-refresh-succeeds", "active")

	failure := errors.New("first job observation failed")
	active := newGenerationTestLane()
	active.refreshErrByJob = map[string]error{"job-refresh-fails": failure}
	m := newGenerationMulti(t, st, active)
	jobs, err := m.ListJobs(context.Background(), "")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %+v, err %v", jobs, err)
	}
	if !errors.Is(active.recordedSyncErr, failure) {
		t.Fatalf("published sync error = %v, want aggregate containing %v", active.recordedSyncErr, failure)
	}
	if _, stale, known := m.SyncInfo(context.Background(), "hpc"); !known || !stale {
		t.Fatalf("freshness known/stale = %t/%t, want true/true", known, stale)
	}
}

func TestListJobsKeepsRemovedTargetHistoryWithoutLiveLane(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-visible", ProjectName: "p", Status: "done", Target: "hpc",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	first := newGenerationTestLane()
	second := newGenerationTestLane()
	m := newGenerationMulti(t, st, first)
	m.RetireTarget("hpc", "generation-a", first)
	m.SetRetiringLane("hpc", "generation-b", second)
	m.RemoveRetiringLane("hpc", "generation-a")
	m.RemoveRetiringLane("hpc", "generation-b")

	jobs, err := m.ListJobs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "job-visible" {
		t.Fatalf("jobs = %+v, want one visible job", jobs)
	}
	if first.listCalls != 0 || second.listCalls != 0 {
		t.Fatalf("list calls first/second = %d/%d, want no live-lane dependency", first.listCalls, second.listCalls)
	}
	detail, err := m.GetJob(ctx, "job-visible")
	if err != nil || detail == nil || detail.Job.ID != "job-visible" {
		t.Fatalf("removed-target detail = %+v, err %v", detail, err)
	}
	if _, err := m.JobResults(ctx, "job-visible"); err != nil {
		t.Fatalf("removed-target results: %v", err)
	}
	if err := m.ArchiveJob(ctx, "job-visible"); err != nil {
		t.Fatalf("archive removed-target history: %v", err)
	}
	if err := m.UnarchiveJob(ctx, "job-visible"); err != nil {
		t.Fatalf("unarchive removed-target history: %v", err)
	}
}

func TestForceRefreshTargetAggregatesEveryLiveGeneration(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	active := newGenerationTestLane()
	active.forceReceipt = &RefreshReceipt{RefreshedAt: 120, Refreshed: true}
	oldA := newGenerationTestLane()
	oldAErr := errors.New("old-a refresh failed")
	oldA.forceReceipt = &RefreshReceipt{
		RefreshedAt: 100, Refreshed: false, Reason: "timeout", RetryAfterSeconds: 15,
	}
	oldA.forceErr = oldAErr
	oldB := newGenerationTestLane()
	oldBErr := errors.New("old-b refresh failed")
	oldB.forceReceipt = &RefreshReceipt{
		RefreshedAt: 80, Refreshed: false, Reason: "min_interval", RetryAfterSeconds: 30,
	}
	oldB.forceErr = oldBErr

	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old-a", oldA)
	m.SetRetiringLane("hpc", "old-b", oldB)

	receipt, err := m.ForceRefreshTarget(context.Background(), "hpc")
	if !errors.Is(err, oldAErr) || !errors.Is(err, oldBErr) {
		t.Fatalf("ForceRefreshTarget error = %v, want both retiring failures", err)
	}
	if active.forceCalls != 1 || oldA.forceCalls != 1 || oldB.forceCalls != 1 {
		t.Fatalf("force calls active/old-a/old-b = %d/%d/%d, want 1/1/1",
			active.forceCalls, oldA.forceCalls, oldB.forceCalls)
	}
	if receipt == nil {
		t.Fatal("ForceRefreshTarget receipt is nil")
	}
	if receipt.Refreshed || receipt.RefreshedAt != 80 || receipt.RetryAfterSeconds != 30 {
		t.Fatalf("receipt = %+v, want refreshed=false, refreshed_at=80, retry_after=30", receipt)
	}
	if receipt.Reason != "timeout; min_interval" {
		t.Fatalf("receipt reason = %q, want %q", receipt.Reason, "timeout; min_interval")
	}
}

func TestReconcileAllAndSyncInfoIncludeRetiringLanes(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	active := newGenerationTestLane()
	active.syncAt = 100
	old := newGenerationTestLane()
	old.syncAt = 80
	old.syncStale = true
	m := newGenerationMulti(t, st, active)
	m.SetRetiringLane("hpc", "old", old)

	if err := m.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active.reconcileCalls != 1 || old.reconcileCalls != 1 {
		t.Fatalf("reconcile calls active/old = %d/%d, want 1/1", active.reconcileCalls, old.reconcileCalls)
	}
	at, stale, known := m.SyncInfo(context.Background(), "hpc")
	if at != 80 || !stale || !known {
		t.Fatalf("SyncInfo = (%d, %t, %t), want (80, true, true)", at, stale, known)
	}
}
