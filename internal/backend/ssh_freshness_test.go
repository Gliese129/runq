package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
)

func newFreshnessSSHBackend(t *testing.T, st *store.Store, submitTemplate string) *SSHBackend {
	t.Helper()
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name: "hpc", SubmitTemplate: submitTemplate,
			SubmitIDRegex: `job ([0-9]+)`, MaxInflight: 1,
		},
		Store: st,
		FS:    rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func TestSSHListObservationFailureMarksReturnedSnapshotStale(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `([0-9]+)`,
			StatusTemplate: "exit 17", MaxInflight: 1,
		},
		Store: st,
		FS:    rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-read-failure", ProjectName: "p", Status: "running", TotalTasks: 1,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "task-read-failure", JobID: "job-read-failure", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "running", ExternalID: "42",
		Target: "hpc", TargetGeneration: be.Generation(), TaskDir: t.TempDir(),
		EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	be.recordTasksSync(nil)
	jobs, err := be.ListJobs(ctx, "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("best-effort list jobs = %+v, err %v", jobs, err)
	}
	row, err := st.GetSyncState(ctx, "hpc", be.tasksSyncResource())
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.LastError == "" {
		t.Fatalf("sync state = %+v, want synchronous observation failure", row)
	}
	if _, stale := be.SyncInfo(ctx); !stale {
		t.Fatal("failed list observation returned data labeled fresh")
	}
}

func TestMultiListObservationFailureMarksReturnedSnapshotStale(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `([0-9]+)`,
			StatusTemplate: "exit 17", MaxInflight: 1,
		},
		Store: st,
		FS:    rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-multi-read-failure", ProjectName: "p", Status: "running", TotalTasks: 1,
		Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "task-multi-read-failure", JobID: "job-multi-read-failure", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "running", ExternalID: "42",
		Target: "hpc", TargetGeneration: be.Generation(), TaskDir: t.TempDir(),
		EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	m, err := NewMultiBackend(map[string]Backend{"hpc": be}, st, "hpc")
	if err != nil {
		t.Fatal(err)
	}
	be.recordTasksSync(nil)
	jobs, err := m.ListJobs(ctx, "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("best-effort multi list jobs = %+v, err %v", jobs, err)
	}
	row, err := st.GetSyncState(ctx, "hpc", be.tasksSyncResource())
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.LastError == "" {
		t.Fatalf("sync state = %+v, want multi-list observation failure", row)
	}
	if _, stale, known := m.SyncInfo(ctx, "hpc"); !known || !stale {
		t.Fatalf("multi list freshness known/stale = %t/%t, want true/true", known, stale)
	}
}

func TestSSHSyncStateIsGenerationSpecific(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	active := newFreshnessSSHBackend(t, st, "new-submit {{run_sh}}")
	retiring := newFreshnessSSHBackend(t, st, "old-submit {{run_sh}}")
	if active.tasksSyncResource() == retiring.tasksSyncResource() {
		t.Fatalf("different generations share sync resource %q", active.tasksSyncResource())
	}

	retiringErr := errors.New("old endpoint unavailable")
	retiring.recordTasksSync(retiringErr)
	active.recordTasksSync(nil)
	ctx := context.Background()
	oldState, err := st.GetSyncState(ctx, "hpc", retiring.tasksSyncResource())
	if err != nil {
		t.Fatal(err)
	}
	newState, err := st.GetSyncState(ctx, "hpc", active.tasksSyncResource())
	if err != nil {
		t.Fatal(err)
	}
	if oldState == nil || oldState.LastError != retiringErr.Error() {
		t.Fatalf("retiring sync state = %+v, want preserved failure", oldState)
	}
	if newState == nil || newState.LastError != "" || newState.LastSuccess == 0 {
		t.Fatalf("active sync state = %+v, want independent success", newState)
	}
}

func TestSSHForcedSyncUsesSchedulerOutcomeWhenMarkersDisabled(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be := newFreshnessSSHBackend(t, st, "submit {{run_sh}}")
	be.recordTasksSync(errors.New("previous failure"))

	// With neither done_dir nor workspace configured, marker detection is
	// disabled. The empty scheduler/status pass succeeds and must determine
	// the forced-pass outcome instead of persisting the marker sentinel.
	be.forcedSync(context.Background())
	row, err := st.GetSyncState(context.Background(), "hpc", be.tasksSyncResource())
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.LastError != "" || row.LastSuccess == 0 {
		t.Fatalf("forced sync state = %+v, want scheduler-derived success", row)
	}
}
