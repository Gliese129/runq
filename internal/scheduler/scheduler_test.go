package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testPool(n int) *resource.GPUPool {
	infos := make([]resource.Info, n)
	for i := range infos {
		infos[i] = resource.Info{Index: i, MemFree: 80000}
	}
	return resource.NewGPUPool(infos)
}

// testStore opens an in-memory SQLite store for testing.
// The scheduler persists state on every dispatch/complete, so tests need a real store.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedProject inserts a minimal project row so that job foreign keys are satisfied.
func seedProject(t *testing.T, s *store.Store, name string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT OR IGNORE INTO projects (name, config_json) VALUES (?, '{}')`, name,
	)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// seedJob inserts a minimal job row so that task foreign keys are satisfied.
func seedJob(t *testing.T, s *store.Store, jobID, project string, totalTasks int) {
	t.Helper()
	seedProject(t, s, project)
	err := s.InsertJob(context.Background(), &store.JobRow{
		ID: jobID, ProjectName: project, ConfigJSON: "{}",
		Status: "pending", TotalTasks: totalTasks, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

// seedTask inserts a minimal task row so the scheduler can update it.
func seedTask(t *testing.T, s *store.Store, task *Task) {
	t.Helper()
	err := s.InsertTask(context.Background(), &store.TaskRow{
		ID: task.ID, JobID: task.JobID, ProjectName: task.ProjectName,
		Command: task.Command, ParamsJSON: "{}", GPUsNeeded: task.GPUsNeeded,
		Status: "pending", MaxRetry: task.MaxRetry, LogPath: task.LogPath,
		WorkingDir: task.WorkingDir, EnqueuedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// TestFrozenJobSkipInTick verifies the sibling-skip filter in tick(). When
// one task in a job is frozen, other pending tasks of the same job should
// be left alone — they'd just self-freeze immediately on their first save.
func TestFrozenJobSkipInTick(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows; freeze cannot register tasks")
	}
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	// Sibling task in the same job, pending. We never dispatch t1 directly —
	// instead we freeze a sleeper PID under t1's JobID so the filter applies.
	// Command is `sleep 30` so the dispatched goroutine doesn't complete and
	// release GPUs before our assertion lands.
	t2 := &Task{
		ID: "t2", JobID: "j1", GPUsNeeded: 1,
		Command: "sleep 30", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t2.log"),
	}
	seedJob(t, st, "j1", "test", 2)
	seedTask(t, st, t2)
	q.Push(t2)

	// Spawn a real sleeper to back the FrozenTask (Freeze requires a live PID
	// it can SIGSTOP). Helper lives in freeze_test.go, same package.
	cmd := startSleeper(t)

	freeze := NewFreezeState()
	freeze.Freeze(FreezeEvent{Reason: "disk_low", TriggerTaskID: "t1"},
		map[string]FrozenTask{
			"t1": {PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j1", NeededBytes: 1 << 30},
		})
	if !freeze.IsFrozen() {
		t.Fatal("setup: freeze didn't take")
	}

	s := New(cfg, q, pool, NewLocalLauncher(exec), st, logger, nil, "", freeze)
	defer s.Shutdown() // cancels runTask goroutines + reaps sleep 30

	// Tick once — t2 should be skipped because j1 has a frozen sibling.
	s.tick()
	if pool.FreeCount() != 4 {
		t.Errorf("after frozen tick: pool free = %d, want 4 (no dispatch)", pool.FreeCount())
	}
	if t2.Status != StatusPending {
		t.Errorf("t2 status = %s, want pending", t2.Status)
	}

	// Release the sibling, then tick — t2 should dispatch now.
	freeze.RemoveTask("t1")
	if freeze.IsFrozen() {
		t.Fatal("setup: RemoveTask should have cleared freeze")
	}
	s.tick()
	if pool.FreeCount() != 3 {
		t.Errorf("after unfrozen tick: pool free = %d, want 3 (t2 dispatched)", pool.FreeCount())
	}
	if t2.Status != StatusRunning {
		t.Errorf("t2 status = %s, want running", t2.Status)
	}
}

// TestBackfillRespectsFreeze guards a real bug we just fixed: backfill used
// to skip only paused tasks, ignoring the freeze filters that head selection
// applied. The result was a small task could slip past a head that didn't
// fit, even though that small task lived on a frozen mount or shared a job
// with a frozen sibling.
//
// Setup:
//   - Pool has 1 GPU.
//   - T_head (4 GPUs, j1) ranks highest but can't fit.
//   - T_small (1 GPU, j2) would normally backfill in.
//   - We freeze a sleeper with JobID=j2. With the fix in place, T_small
//     must NOT be dispatched. Without it, backfill would pick T_small.
func TestBackfillRespectsFreeze(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(1) // only 1 GPU — head can't fit
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()

	tHead := &Task{
		ID: "t-head", JobID: "j1", GPUsNeeded: 4,
		Command: "sleep 30", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-head.log"),
	}
	tSmall := &Task{
		ID: "t-small", JobID: "j2", GPUsNeeded: 1,
		Command: "sleep 30", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-small.log"),
	}
	seedJob(t, st, "j1", "test", 1)
	seedJob(t, st, "j2", "test", 1)
	seedTask(t, st, tHead)
	seedTask(t, st, tSmall)
	q.Push(tHead)
	q.Push(tSmall)

	// Freeze a sleeper under j2 — should keep t-small from being backfilled.
	cmd := startSleeper(t)
	freeze := NewFreezeState()
	freeze.Freeze(FreezeEvent{Reason: "disk_low", TriggerTaskID: "phantom"},
		map[string]FrozenTask{
			"phantom": {PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j2", NeededBytes: 1 << 30},
		})

	s := New(cfg, q, pool, NewLocalLauncher(exec), st, logger, nil, "", freeze)
	defer s.Shutdown()

	s.tick()
	if pool.FreeCount() != 1 {
		t.Errorf("frozen tick: pool free = %d, want 1 (no dispatch, backfill filtered)", pool.FreeCount())
	}
	if tSmall.Status != StatusPending {
		t.Errorf("t-small status = %s, want pending — backfill bypassed freeze filter", tSmall.Status)
	}

	// Unfreeze j2 → backfill should pick t-small on next tick.
	freeze.RemoveTask("phantom")
	s.tick()
	if pool.FreeCount() != 0 {
		t.Errorf("after unfreeze: pool free = %d, want 0 (t-small dispatched via backfill)", pool.FreeCount())
	}
	if tSmall.Status != StatusRunning {
		t.Errorf("t-small status = %s, want running", tSmall.Status)
	}
}

// TestFrozenMountSkipInTick verifies the mount-based dispatch filter. Uses
// t.TempDir() + live utils.MountOf so the test is portable across CI
// environments — whatever mount actually covers TempDir becomes the
// "frozen" mount, and we set the pending task's CheckpointDir to the same
// directory.
func TestFrozenMountSkipInTick(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	dir := t.TempDir()
	parts, err := utils.LoadMountTable()
	if err != nil {
		t.Fatalf("LoadMountTable: %v", err)
	}
	mount := utils.MountOf(dir, parts)
	if mount == "" {
		t.Skipf("could not resolve mount for %q on this machine — partition table didn't match", dir)
	}

	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	pendingTask := &Task{
		ID: "t-pending", JobID: "j-other", GPUsNeeded: 1,
		Command:       "sleep 30",
		WorkingDir:    dir,
		LogPath:       filepath.Join(dir, "t-pending.log"),
		CheckpointDir: filepath.Join(dir, "ckpts"), // resolves to `mount`
	}
	seedJob(t, st, "j-other", "test", 1)
	seedTask(t, st, pendingTask)
	q.Push(pendingTask)

	// Freeze a sleeper on the same mount but a DIFFERENT job (so this
	// exercises the mount filter, not the job filter).
	cmd := startSleeper(t)
	freeze := NewFreezeState()
	freeze.Freeze(FreezeEvent{Reason: "disk_low", TriggerTaskID: "phantom"},
		map[string]FrozenTask{
			"phantom": {PID: cmd.Process.Pid, Mount: mount, JobID: "j-frozen", NeededBytes: 1 << 30},
		})

	s := New(DefaultConfig(), q, pool, NewLocalLauncher(exec), st, logger, nil, "", freeze)
	defer s.Shutdown()

	s.tick()
	if pendingTask.Status != StatusPending {
		t.Errorf("pending task dispatched despite frozen mount %q (status=%s)", mount, pendingTask.Status)
	}
	if pool.FreeCount() != 4 {
		t.Errorf("pool free = %d, want 4 (no dispatch)", pool.FreeCount())
	}

	// Unfreeze the mount → pending task should dispatch.
	freeze.RemoveTask("phantom")
	s.tick()
	if pendingTask.Status != StatusRunning {
		t.Errorf("pending task should dispatch after mount unfreeze (status=%s)", pendingTask.Status)
	}
}

func TestSchedulerDispatchSingle(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	task := &Task{
		ID: "t1", JobID: "j1", GPUsNeeded: 1,
		Command: `echo "dispatched"`, WorkingDir: dir,
		LogPath: filepath.Join(dir, "t1.log"),
	}
	seedJob(t, st, "j1", "test", 1)
	seedTask(t, st, task)

	s := New(cfg, q, pool, NewLocalLauncher(exec), st, logger, nil, "", nil)
	s.Start()
	q.Push(task)

	time.Sleep(1 * time.Second)
	s.Shutdown()

	got := q.Get("t1")
	if got.Status != StatusSuccess {
		t.Errorf("expected success, got %s", got.Status)
	}
	if pool.FreeCount() != 4 {
		t.Errorf("expected 4 free GPUs, got %d", pool.FreeCount())
	}
}

func TestSchedulerRetry(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(2)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond

	task := &Task{
		ID: "t-fail", JobID: "j1", GPUsNeeded: 1,
		Command: "exit 1", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-fail.log"), MaxRetry: 2,
	}
	seedJob(t, st, "j1", "test", 1)
	seedTask(t, st, task)

	s := New(cfg, q, pool, NewLocalLauncher(exec), st, logger, nil, "", nil)
	s.Start()
	q.Push(task)

	time.Sleep(2 * time.Second)
	s.Shutdown()

	got := q.Get("t-fail")
	if got.Status != StatusFailed {
		t.Errorf("expected failed, got %s", got.Status)
	}
	if got.RetryCount != 2 {
		t.Errorf("expected 2 retries, got %d", got.RetryCount)
	}
}

func TestHandleFailureLogsNextRetryOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	q := NewQueue()
	st := testStore(t)
	task := &Task{
		ID: "t-fail", JobID: "j1", ProjectName: "test",
		Command: "exit 1", GPUsNeeded: 1, MaxRetry: 1,
	}
	seedJob(t, st, "j1", "test", 1)
	seedTask(t, st, task)
	q.Push(task)

	s := New(DefaultConfig(), q, testPool(1), NewLocalLauncher(executor.New()), st, logger, nil, "", nil)
	s.handleFailure(task)

	logs := buf.String()
	if !strings.Contains(logs, "retry=1") {
		t.Fatalf("expected retry=1 in logs, got %q", logs)
	}
	if strings.Contains(logs, "retry=2") {
		t.Fatalf("unexpected retry=2 in logs: %q", logs)
	}

	row, err := st.GetTask(context.Background(), "t-fail")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if row.RetryCount != 1 {
		t.Fatalf("expected DB retry_count 1, got %d", row.RetryCount)
	}
	if got := q.Get("t-fail").RetryCount; got != 1 {
		t.Fatalf("expected queue retry count 1, got %d", got)
	}
}

func TestSchedulerBackfill(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.BackfillEnabled = true
	cfg.AgingThreshold = 1 * time.Hour

	big := &Task{
		ID: "t-big", JobID: "j1", GPUsNeeded: 8,
		Command: "echo big", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-big.log"),
	}
	small := &Task{
		ID: "t-small", JobID: "j1", GPUsNeeded: 1,
		Command: "echo small", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-small.log"),
	}
	seedJob(t, st, "j1", "test", 2)
	seedTask(t, st, big)
	seedTask(t, st, small)

	s := New(cfg, q, pool, NewLocalLauncher(exec), st, logger, nil, "", nil)
	s.Start()
	q.Push(big)
	q.Push(small)

	time.Sleep(1 * time.Second)
	s.Shutdown()

	gotSmall := q.Get("t-small")
	if gotSmall.Status != StatusSuccess {
		t.Errorf("expected small task success, got %s", gotSmall.Status)
	}
	gotBig := q.Get("t-big")
	if gotBig.Status != StatusPending {
		t.Errorf("expected big task pending, got %s", gotBig.Status)
	}
}

func TestSchedulerReservation(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue()
	pool := testPool(4)
	exec := executor.New()
	st := testStore(t)
	logger := testLogger()

	cfg := DefaultConfig()
	cfg.TickInterval = 50 * time.Millisecond
	cfg.BackfillEnabled = true
	cfg.AgingThreshold = 1 * time.Millisecond

	big := &Task{
		ID: "t-big", JobID: "j1", GPUsNeeded: 4,
		Command: "echo big", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-big.log"),
	}
	small := &Task{
		ID: "t-small", JobID: "j1", GPUsNeeded: 1,
		Command: "echo small", WorkingDir: dir,
		LogPath: filepath.Join(dir, "t-small.log"),
	}
	seedJob(t, st, "j1", "test", 2)
	seedTask(t, st, big)
	seedTask(t, st, small)

	// Occupy 2 GPUs so big task can't fit.
	pool.Allocate(2, "external-task")

	s := New(cfg, q, pool, NewLocalLauncher(exec), st, logger, nil, "", nil)
	s.Start()
	q.Push(big)
	q.Push(small)

	// Reservation mode should block backfill.
	time.Sleep(500 * time.Millisecond)
	gotSmall := q.Get("t-small")
	if gotSmall.Status != StatusPending {
		t.Errorf("expected small task blocked by reservation, got %s", gotSmall.Status)
	}

	// Free external GPUs → big task gets scheduled.
	pool.Release("external-task")
	time.Sleep(1 * time.Second)
	s.Shutdown()

	gotBig := q.Get("t-big")
	if gotBig.Status != StatusSuccess {
		t.Errorf("expected big task success, got %s", gotBig.Status)
	}
}
