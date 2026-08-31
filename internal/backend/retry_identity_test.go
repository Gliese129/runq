package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/scheduler"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/utils"
)

type failingWriteFS struct {
	rfs.FS
	err error
}

func (f failingWriteFS) WriteFile(string, []byte, os.FileMode) error { return f.err }

type observingWriteFS struct {
	rfs.FS
	onWrite func()
}

func (f observingWriteFS) WriteFile(name string, data []byte, mode os.FileMode) error {
	if f.onWrite != nil {
		f.onWrite()
	}
	return f.FS.WriteFile(name, data, mode)
}

func TestSSHManualRetryLaunchesFreshAttemptHandle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	taskDir := t.TempDir()
	attemptFile := filepath.Join(taskDir, "attempt.txt")
	submitCmd := "printf '%s\\n' \"$RUNQ_SUBMIT_HANDLE\" > " + utils.ShellQuote(attemptFile) +
		"\nprintf 'job %s\\n' \"$RUNQ_SUBMIT_HANDLE\"\n"
	if err := os.WriteFile(filepath.Join(taskDir, "submit.cmd"), []byte(submitCmd), 0o600); err != nil {
		t.Fatalf("write submit command: %v", err)
	}
	if err := os.WriteFile(attemptFile, []byte("task-1-a0\n"), 0o644); err != nil {
		t.Fatalf("seed prior attempt: %v", err)
	}

	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name:           "hpc",
			SubmitTemplate: "unused {{run_sh}}",
			SubmitIDRegex:  `job ([A-Za-z0-9._-]+)`,
			MaxInflight:    1,
		},
		Store: st,
		FS:    rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatalf("new SSH backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-1", ProjectName: "p", ConfigJSON: "{}",
		Status: "failed", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	row := &store.TaskRow{
		ID: "task-1", JobID: "job-1", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "failed", RetryCount: 0, MaxRetry: 3,
		TaskDir: taskDir, Target: "hpc", TargetGeneration: be.Generation(),
		EnqueuedAt: time.Now(),
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	queued := TaskRowToSchedulerTask(row)
	be.queue.Push(queued)
	if err := be.queue.MarkRunning(row.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := be.queue.Complete(row.ID, scheduler.StatusFailed); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := be.RetryTask(ctx, row.ID); err != nil {
		t.Fatalf("manual retry: %v", err)
	}
	retried, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatalf("get retried task: %v", err)
	}
	if retried.RetryCount != 1 {
		t.Fatalf("persisted retry count = %d, want 1", retried.RetryCount)
	}
	queued = be.queue.Get(row.ID)
	if queued == nil || queued.RetryCount != 1 {
		t.Fatalf("queued retry epoch = %+v, want retry_count 1", queued)
	}

	if err := be.launcher.Launch(ctx, queued); err != nil {
		t.Fatalf("launch retried attempt: %v", err)
	}
	launched, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatalf("get launched task: %v", err)
	}
	if launched.ExternalID != "task-1-a1" {
		t.Fatalf("retried external id = %q, want task-1-a1", launched.ExternalID)
	}
	gotAttempt, err := os.ReadFile(attemptFile)
	if err != nil {
		t.Fatalf("read launched attempt: %v", err)
	}
	if string(gotAttempt) != "task-1-a1\n" {
		t.Fatalf("executed attempt handle = %q, want task-1-a1", gotAttempt)
	}
}

func TestSSHRestoreNeverResubmitsDurableSubmittingAttempt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name: "hpc", SubmitTemplate: "submit {{run_sh}}",
			SubmitIDRegex: `job ([0-9]+)`, MaxInflight: 1,
		},
		Store: st,
		FS:    rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatalf("new SSH backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-submitting", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "task-submitting", JobID: "job-submitting", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "submitting",
		Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	if err := be.restoreLane(ctx); err != nil {
		t.Fatalf("restore lane: %v", err)
	}

	row, err := st.GetTask(ctx, "task-submitting")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.Status != "unknown" || row.StatusSource != "submit" {
		t.Fatalf("recovered status/source = %q/%q, want unknown/submit", row.Status, row.StatusSource)
	}
	if !strings.Contains(row.FailureDetail, "submission was in flight") {
		t.Fatalf("recovery evidence missing: %q", row.FailureDetail)
	}
	queued := be.queue.Get(row.ID)
	if queued == nil || queued.Status != scheduler.StatusUnknown {
		t.Fatalf("recovered attempt became launchable: %+v", queued)
	}
	if be.pool.FreeCount() != 0 {
		t.Fatal("recovered ambiguous attempt did not retain its submission slot")
	}
}

func TestSSHRestoreFailsClosedWhenStoreIsUnreadable(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatalf("new SSH backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if err := be.restoreLane(context.Background()); err == nil {
		t.Fatal("restore succeeded with an unreadable durable store")
	}
	if be.queue.Len() != 0 {
		t.Fatalf("failed restore published %d queue entries", be.queue.Len())
	}
}

func TestSSHRestoreReinstatesPausedDispatchGate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{ID: "job-paused", ProjectName: "p", ConfigJSON: "{}", Status: "paused", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, &store.TaskRow{ID: "task-paused", JobID: "job-paused", ProjectName: "p", Command: "true", ParamsJSON: "{}", Status: "pending", Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := be.restoreLane(ctx); err != nil {
		t.Fatalf("restore lane: %v", err)
	}
	if !be.sched.IsJobPaused("job-paused") {
		t.Fatal("paused job dispatch gate was not restored")
	}
	if !be.Capabilities().PauseResume {
		t.Fatal("SSH lane hides its runq-level pause/resume capability")
	}
}

func TestSSHRestorePreservesPendingExternalKillForReplay(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, MaxInflight: 1},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-external-kill", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	row := &store.TaskRow{
		ID: "task-external-kill", JobID: "job-external-kill", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "pending",
		ExternalID: "scheduler-42", KillRequested: true,
		Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now(),
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}

	if err := be.restoreLane(ctx); err != nil {
		t.Fatalf("restore lane: %v", err)
	}
	restored, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != "pending" || restored.ExternalID != row.ExternalID || !restored.KillRequested {
		t.Fatalf("durable task after recovery = %+v, want pending external work with kill intent", restored)
	}
	if queued := be.queue.Get(row.ID); queued == nil || queued.Status != scheduler.StatusRunning {
		t.Fatalf("queue did not restore external ownership: %+v", queued)
	}
	if be.pool.FreeCount() != 0 {
		t.Fatal("external work did not retain its submission slot")
	}

	// The restored in-memory kill flag must still win at the next verdict.
	be.sched.FinishTask(be.queue.Get(row.ID), scheduler.StatusFailed, map[string]any{"status_source": "probe"})
	settled, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != "killed" {
		t.Fatalf("restored kill intent settled as %q, want killed", settled.Status)
	}
}

func TestSSHResumeJobCompletesGenerationFanoutAfterPeerPersistence(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-resume-fanout", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	be.sched.PauseJob("job-resume-fanout")

	// Another owning generation already changed the shared durable status.
	// This lane must still remove its local dispatch gate idempotently.
	if err := be.ResumeJob(ctx, "job-resume-fanout"); err != nil {
		t.Fatalf("complete resume fanout: %v", err)
	}
	if be.sched.IsJobPaused("job-resume-fanout") {
		t.Fatal("resume fanout left this generation's dispatch gate paused")
	}
}

func TestSSHPausePersistenceFailureReopensDispatchGate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-pause-fail", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`
		CREATE TRIGGER reject_job_pause
		BEFORE UPDATE OF status ON jobs
		WHEN NEW.status = 'paused'
		BEGIN SELECT RAISE(ABORT, 'injected pause failure'); END;
	`); err != nil {
		t.Fatal(err)
	}

	if err := be.PauseJob(ctx, "job-pause-fail"); err == nil {
		t.Fatal("pause unexpectedly succeeded despite persistence failure")
	}
	if be.sched.IsJobPaused("job-pause-fail") {
		t.Fatal("failed pause left the dispatch gate closed")
	}
}

func TestSSHKillJobClearsPausedDispatchGate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{ID: "job-paused-kill", ProjectName: "p", ConfigJSON: "{}", Status: "paused", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	row := &store.TaskRow{ID: "task-paused-kill", JobID: "job-paused-kill", ProjectName: "p", Command: "true", ParamsJSON: "{}", Status: "pending", TaskDir: t.TempDir(), Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now()}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	be.queue.Push(TaskRowToSchedulerTask(row))
	be.sched.PauseJob(row.JobID)

	if err := be.KillJob(ctx, row.JobID); err != nil {
		t.Fatalf("kill paused job: %v", err)
	}
	if be.sched.IsJobPaused(row.JobID) {
		t.Fatal("whole-job kill retained the paused dispatch gate")
	}
	job, err := st.GetJob(ctx, row.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.Status != "killed" {
		t.Fatalf("job after kill = %+v, want killed", job)
	}
}

func TestSSHManualRetryIsSerializedAndConsumesOneEpoch(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatalf("new SSH backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	taskDir := t.TempDir()
	if err := st.InsertJob(ctx, &store.JobRow{ID: "job-race", ProjectName: "p", ConfigJSON: "{}", Status: "failed", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	row := &store.TaskRow{ID: "task-race", JobID: "job-race", ProjectName: "p", Command: "true", ParamsJSON: "{}", Status: "failed", MaxRetry: 3, TaskDir: taskDir, Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now()}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	queued := TaskRowToSchedulerTask(row)
	be.queue.Push(queued)
	if err := be.queue.MarkRunning(row.ID); err != nil {
		t.Fatal(err)
	}
	if err := be.queue.Complete(row.ID, scheduler.StatusFailed); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	for range 2 {
		go func() { results <- be.RetryTask(ctx, row.ID) }()
	}
	var successes int
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent retries = %d, want exactly 1", successes)
	}
	got, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" || got.RetryCount != 1 {
		t.Fatalf("durable retry = %s epoch %d, want pending epoch 1", got.Status, got.RetryCount)
	}
}

func TestSSHManualRetryParticipatesInAdmissionBarrier(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	localFS := rfs.NewLocalFS()
	var be *SSHBackend
	observed := make(chan bool, 1)
	fsys := observingWriteFS{FS: localFS, onWrite: func() {
		observed <- be.HasInFlightAdmissions()
	}}
	be, err = NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     fsys,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	row := &store.TaskRow{
		ID: "task-admission", JobID: "job-admission", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "failed", TaskDir: t.TempDir(),
		Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now(),
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: row.JobID, ProjectName: "p", ConfigJSON: "{}", Status: "failed",
		TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	queued := TaskRowToSchedulerTask(row)
	be.queue.Push(queued)
	_ = be.queue.MarkRunning(row.ID)
	_ = be.queue.Complete(row.ID, scheduler.StatusFailed)

	if err := be.RetryTask(ctx, row.ID); err != nil {
		t.Fatalf("manual retry: %v", err)
	}
	select {
	case inFlight := <-observed:
		if !inFlight {
			t.Fatal("manual retry reset ran outside the admission barrier")
		}
	default:
		t.Fatal("manual retry did not reach wrapper reset")
	}
	if be.HasInFlightAdmissions() {
		t.Fatal("manual retry left the admission barrier held after return")
	}
}

func TestSSHManualRetryWaitsForSharedTaskAttemptLock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	row := &store.TaskRow{
		ID: "task-attempt-lock", JobID: "job-attempt-lock", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "failed", TaskDir: t.TempDir(),
		Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now(),
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: row.JobID, ProjectName: "p", ConfigJSON: "{}", Status: "failed",
		TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	queued := TaskRowToSchedulerTask(row)
	be.queue.Push(queued)
	_ = be.queue.MarkRunning(row.ID)
	_ = be.queue.Complete(row.ID, scheduler.StatusFailed)

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan struct{})
	go func() {
		_ = st.WithTaskAttemptLock(row.ID, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
		close(lockDone)
	}()
	<-lockHeld

	retryDone := make(chan error, 1)
	go func() { retryDone <- be.RetryTask(ctx, row.ID) }()
	deadline := time.Now().Add(5 * time.Second)
	for !be.HasInFlightAdmissions() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !be.HasInFlightAdmissions() {
		t.Fatal("retry did not enter the admission barrier while waiting for task-attempt lock")
	}
	stillFailed, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillFailed.Status != "failed" {
		t.Fatalf("retry durable intent advanced before task-attempt lock: %q", stillFailed.Status)
	}

	close(releaseLock)
	<-lockDone
	if err := <-retryDone; err != nil {
		t.Fatalf("manual retry after task-attempt lock release: %v", err)
	}
	if be.HasInFlightAdmissions() {
		t.Fatal("retry drain barrier remained held after task-attempt sequence completed")
	}
}

func TestSSHRestoreRetryResetWaitsForSharedTaskAttemptLock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatal(err)
	}
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	row := &store.TaskRow{
		ID: "task-restore-attempt-lock", JobID: "job-restore-attempt-lock", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "failed", TaskDir: t.TempDir(),
		Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now(),
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: row.JobID, ProjectName: "p", ConfigJSON: "{}", Status: "failed",
		TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginTaskRetry(ctx, row.ID, 0, be.Generation()); err != nil {
		t.Fatal(err)
	}

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	go func() {
		_ = st.WithTaskAttemptLock(row.ID, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld
	restoreDone := make(chan error, 1)
	go func() { restoreDone <- be.restoreLane(ctx) }()

	select {
	case err := <-restoreDone:
		t.Fatalf("restore bypassed task-attempt lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	intent, err := st.GetTask(ctx, row.ID)
	if err != nil || intent == nil {
		t.Fatalf("read retry intent: %v", err)
	}
	if intent.Status != "submitting" || intent.StatusSource != "retry" {
		t.Fatalf("restore advanced while lock held: %#v", intent)
	}

	close(releaseLock)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore retry reset: %v", err)
	}
	restored, err := st.GetTask(ctx, row.ID)
	if err != nil || restored == nil {
		t.Fatalf("read restored task: %v", err)
	}
	if restored.Status != "pending" || restored.StatusSource != "" || restored.RetryCount != 1 {
		t.Fatalf("restored retry = %#v", restored)
	}
}

func TestSSHManualRetryResumesDurableResetIntent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	localFS := rfs.NewLocalFS()
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     localFS,
	})
	if err != nil {
		t.Fatalf("new SSH backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	row := &store.TaskRow{ID: "task-reset", JobID: "job-reset", ProjectName: "p", Command: "true", ParamsJSON: "{}", Status: "failed", MaxRetry: 3, TaskDir: t.TempDir(), Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now()}
	if err := st.InsertJob(ctx, &store.JobRow{ID: row.JobID, ProjectName: "p", ConfigJSON: "{}", Status: "failed", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	queued := TaskRowToSchedulerTask(row)
	be.queue.Push(queued)
	_ = be.queue.MarkRunning(row.ID)
	_ = be.queue.Complete(row.ID, scheduler.StatusFailed)

	resetErr := errors.New("injected wrapper write failure")
	be.backend.FS = failingWriteFS{FS: localFS, err: resetErr}
	if err := be.RetryTask(ctx, row.ID); !errors.Is(err, resetErr) {
		t.Fatalf("retry error = %v, want injected reset error", err)
	}
	intent, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Status != "submitting" || intent.StatusSource != "retry" || intent.RetryCount != 1 {
		t.Fatalf("retry intent = %s/%s epoch %d, want submitting/retry epoch 1", intent.Status, intent.StatusSource, intent.RetryCount)
	}
	if got := be.queue.Get(row.ID); got == nil || got.Status != scheduler.StatusFailed {
		t.Fatalf("failed reset published queue state: %+v", got)
	}

	be.backend.FS = localFS
	if err := be.RetryTask(ctx, row.ID); err != nil {
		t.Fatalf("resume retry intent: %v", err)
	}
	completed, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "pending" || completed.RetryCount != 1 {
		t.Fatalf("resumed retry = %s epoch %d, want pending epoch 1", completed.Status, completed.RetryCount)
	}
}

func TestSSHKillJobSettlesUnknownAttemptWithoutExternalID(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{
			Name: "hpc", SubmitTemplate: "submit {{run_sh}}",
			SubmitIDRegex: `job ([0-9]+)`, MaxInflight: 1,
		},
		Store: st,
		FS:    rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatalf("new SSH backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "job-unknown", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	row := &store.TaskRow{
		ID: "task-unknown", JobID: "job-unknown", ProjectName: "p",
		Command: "true", ParamsJSON: "{}", Status: "unknown",
		StatusSource: "submit", TaskDir: t.TempDir(), Target: "hpc",
		TargetGeneration: be.Generation(), EnqueuedAt: time.Now(),
	}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	queued := TaskRowToSchedulerTask(row)
	queued.Status = scheduler.StatusUnknown
	be.queue.Restore(queued)
	be.pool.Reserve(row.ID)

	if err := be.KillJob(ctx, row.JobID); err != nil {
		t.Fatalf("kill job: %v", err)
	}
	got, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "killed" || got.StatusSource != "runq" {
		t.Fatalf("unknown task after whole-job kill = %q/%q, want killed/runq", got.Status, got.StatusSource)
	}
	if be.pool.FreeCount() != 1 {
		t.Fatal("whole-job kill did not release the unknown attempt's slot")
	}
}

func TestSSHKillTaskReportsPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{ID: "job-kill-fail", ProjectName: "p", ConfigJSON: "{}", Status: "pending", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	row := &store.TaskRow{ID: "task-kill-fail", JobID: "job-kill-fail", ProjectName: "p", Command: "true", ParamsJSON: "{}", Status: "pending", TaskDir: t.TempDir(), Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now()}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	be.queue.Push(TaskRowToSchedulerTask(row))
	if _, err := st.DB().Exec(`
		CREATE TRIGGER reject_killed_update
		BEFORE UPDATE OF status ON tasks
		WHEN NEW.status = 'killed'
		BEGIN SELECT RAISE(ABORT, 'injected killed persistence failure'); END`); err != nil {
		t.Fatal(err)
	}

	if err := be.KillTask(ctx, row.ID); err == nil {
		t.Fatal("kill acknowledged despite rejected durable transition")
	}
	got, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("failed kill changed durable status to %q", got.Status)
	}
	if qt := be.queue.Get(row.ID); qt == nil || qt.Status != scheduler.StatusPending {
		t.Fatalf("failed kill changed queue state: %+v", qt)
	}
}

func TestSSHRestoreReplaysDurableKillIntentIntoLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	be, err := NewSSHBackend(SSHBackendConfig{
		Target: config.TargetConfig{Name: "hpc", SubmitTemplate: "submit {{run_sh}}", SubmitIDRegex: `job ([0-9]+)`, MaxInflight: 1},
		Store:  st,
		FS:     rfs.NewLocalFS(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Close() })
	if err := st.InsertJob(ctx, &store.JobRow{ID: "job-kill-recover", ProjectName: "p", ConfigJSON: "{}", Status: "running", TotalTasks: 1, Target: "hpc", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	row := &store.TaskRow{ID: "task-kill-recover", JobID: "job-kill-recover", ProjectName: "p", Command: "true", ParamsJSON: "{}", Status: "running", KillRequested: true, ExternalID: "external-1", MaxRetry: 3, TaskDir: t.TempDir(), Target: "hpc", TargetGeneration: be.Generation(), EnqueuedAt: time.Now()}
	if err := st.InsertTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := be.restoreLane(ctx); err != nil {
		t.Fatalf("restore lane: %v", err)
	}
	queued := be.queue.Get(row.ID)
	if queued == nil || queued.Status != scheduler.StatusRunning {
		t.Fatalf("restored queue entry = %+v", queued)
	}
	be.sched.FinishTask(queued, scheduler.StatusFailed, map[string]any{"status_source": "probe"})
	got, err := st.GetTask(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "killed" || got.KillRequested {
		t.Fatalf("restored kill intent settled as status=%q kill_requested=%v", got.Status, got.KillRequested)
	}
}
