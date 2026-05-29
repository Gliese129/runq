package scheduler

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ── tests for stubs-only behavior (pass on NewFreezeState alone) ─────────

func TestFreezeStateHostInitialized(t *testing.T) {
	fs := NewFreezeState()
	want, _ := os.Hostname()
	if got := fs.Host(); got != want {
		t.Errorf("Host() = %q, want %q", got, want)
	}
}

func TestFreezeStateInitiallyUnfrozen(t *testing.T) {
	fs := NewFreezeState()
	if fs.IsFrozen() {
		t.Error("brand-new FreezeState should not be frozen")
	}
	if got := fs.FrozenTaskIDs(); len(got) != 0 {
		t.Errorf("FrozenTaskIDs should be empty, got %v", got)
	}
	if got := fs.Events(); len(got) != 0 {
		t.Errorf("Events should be empty, got %v", got)
	}
	if ev := fs.LatestEvent(); ev != nil {
		t.Errorf("LatestEvent should be nil, got %+v", ev)
	}
}

// ── tests for core impl (pass once stage 1 core lands) ────────────────────

// TestFreezeStopAndThaw is the integration test for SIGSTOP/SIGCONT broadcast.
// Spawns two real sleep subprocesses, freezes them, verifies they enter
// stopped state, thaws, verifies they resume.
//
// Skipped on Windows (no SIGSTOP).
func TestFreezeStopAndThaw(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}

	cmd1 := startSleeper(t)
	cmd2 := startSleeper(t)

	fs := NewFreezeState()
	tasks := map[string]FrozenTask{
		"t1": {PID: cmd1.Process.Pid, Mount: "/tmp", JobID: "j1", NeededBytes: 1},
		"t2": {PID: cmd2.Process.Pid, Mount: "/tmp", JobID: "j1", NeededBytes: 1},
	}

	fs.Freeze(FreezeEvent{Reason: "disk_low"}, tasks)
	if !fs.IsFrozen() {
		t.Fatal("IsFrozen=false after Freeze")
	}
	if got := fs.Events(); len(got) != 1 || got[0].Reason != "disk_low" {
		t.Errorf("expected one disk_low event, got %+v", got)
	}
	waitForState(t, cmd1.Process.Pid, true)
	waitForState(t, cmd2.Process.Pid, true)

	// Use ThawForce — we want to release both regardless of disk state.
	thawed := fs.ThawForce(fs.FrozenTaskIDs())
	if len(thawed) != 2 {
		t.Errorf("expected 2 thawed tasks, got %d (%v)", len(thawed), thawed)
	}
	if fs.IsFrozen() {
		t.Error("IsFrozen=true after Thaw")
	}
	waitForState(t, cmd1.Process.Pid, false)
	waitForState(t, cmd2.Process.Pid, false)
}

// TestThawIdempotent: thawing an already-thawed FreezeState is allowed and
// returns an empty slice. The CLI command and auto-thaw goroutine both depend
// on this being safe to call unconditionally.
func TestThawIdempotent(t *testing.T) {
	fs := NewFreezeState()
	thawed := fs.ThawForce(nil)
	if len(thawed) != 0 {
		t.Errorf("expected empty thawed slice, got %v", thawed)
	}
}

// TestFreezeEventsAccumulate confirms that repeated triggers (e.g. multiple
// tasks each emitting disk_low before user thaws) all land in Events(), even
// when Reason strings collide. Webhook + audit rely on this.
func TestFreezeEventsAccumulate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows; Freeze rejects entries")
	}
	cmd1 := startSleeper(t)
	cmd2 := startSleeper(t)

	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{
		Reason:        "disk_low",
		TriggerTaskID: "t1",
		FreeBytes:     1000,
		NeededEst:     2000,
	}, map[string]FrozenTask{"t1": {PID: cmd1.Process.Pid, Mount: "/tmp", JobID: "j1", NeededBytes: 2000}})

	fs.Freeze(FreezeEvent{
		Reason:        "disk_low",
		TriggerTaskID: "t2",
		FreeBytes:     500,
		NeededEst:     2000,
	}, map[string]FrozenTask{"t2": {PID: cmd2.Process.Pid, Mount: "/tmp", JobID: "j1", NeededBytes: 2000}})

	events := fs.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 accumulated events, got %d", len(events))
	}
	if events[0].TriggerTaskID != "t1" || events[1].TriggerTaskID != "t2" {
		t.Errorf("event order wrong: %+v", events)
	}
	if events[0].FreeBytes != 1000 || events[1].FreeBytes != 500 {
		t.Errorf("event payloads dropped: %+v", events)
	}

	triggers := fs.TriggerTaskIDs()
	if len(triggers) != 2 {
		t.Errorf("TriggerTaskIDs dedup wrong, got %v", triggers)
	}

	if latest := fs.LatestEvent(); latest == nil || latest.TriggerTaskID != "t2" {
		t.Errorf("LatestEvent should be t2, got %+v", latest)
	}

	// After Thaw, events must reset.
	fs.ThawForce(fs.FrozenTaskIDs())
	if got := fs.Events(); len(got) != 0 {
		t.Errorf("Thaw should clear events, got %+v", got)
	}
	if ev := fs.LatestEvent(); ev != nil {
		t.Errorf("LatestEvent after Thaw should be nil, got %+v", ev)
	}
}

// TestFreezeEventEnteredAtAutoStamped confirms that callers leaving EnteredAt
// zero get it auto-filled inside Freeze. This is the contract: most callers
// don't construct their own timestamp.
func TestFreezeEventEnteredAtAutoStamped(t *testing.T) {
	fs := NewFreezeState()
	before := time.Now()
	// Empty tasks map still records the event (audit trail), no SIGSTOP needed.
	fs.Freeze(FreezeEvent{Reason: "test", TriggerTaskID: "t1"}, map[string]FrozenTask{})
	after := time.Now()

	ev := fs.LatestEvent()
	if ev == nil {
		t.Fatal("no event recorded")
	}
	if ev.EnteredAt.Before(before) || ev.EnteredAt.After(after) {
		t.Errorf("EnteredAt %v outside [%v, %v]", ev.EnteredAt, before, after)
	}
}

// TestAutoThawOnLastTaskRemoved guards the auto-thaw edge case: if the user
// kills every frozen task, the scheduler must not stay paused forever. The
// implementation has to release mu before calling Thaw to avoid the obvious
// deadlock.
func TestAutoThawOnLastTaskRemoved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	cmd1 := startSleeper(t)
	cmd2 := startSleeper(t)

	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "test", TriggerTaskID: "t1"},
		map[string]FrozenTask{"t1": {PID: cmd1.Process.Pid, Mount: "/tmp", JobID: "j1"}})
	fs.Freeze(FreezeEvent{Reason: "test", TriggerTaskID: "t2"},
		map[string]FrozenTask{"t2": {PID: cmd2.Process.Pid, Mount: "/tmp", JobID: "j1"}})
	if got := fs.Events(); len(got) != 2 {
		t.Errorf("expected 2 events accumulated, got %d", len(got))
	}
	if !fs.IsFrozen() {
		t.Fatal("not frozen after Freeze")
	}

	fs.RemoveTask("t1")
	if !fs.IsFrozen() {
		t.Error("still have t2; should remain frozen")
	}

	fs.RemoveTask("t2")
	if fs.IsFrozen() {
		t.Error("last task removed → expected auto-thaw")
	}

	// RemoveTask is pure state cleanup — it doesn't SIGCONT. The sleepers
	// remain stopped; t.Cleanup will kill them via cmd.Process.Kill which
	// works through SIGSTOP.
	_ = cmd1
	_ = cmd2
}

// TestThawTasksPerMount verifies per-mount partial thaw. Three frozen tasks
// across two mounts; only the mount whose free bytes exceed the per-task
// threshold should release its tasks. The other mount's tasks stay frozen
// and appear in result.Blocked with FreeBytes / Threshold populated.
//
// Uses a fake diskUsage closure so the test is hermetic — no real disk
// state matters.
func TestThawTasksPerMount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	cmd1 := startSleeper(t)
	cmd2 := startSleeper(t)
	cmd3 := startSleeper(t)

	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "disk_low"}, map[string]FrozenTask{
		"t1": {PID: cmd1.Process.Pid, Mount: "/disk_a", JobID: "j1", NeededBytes: 5 << 30},
		"t2": {PID: cmd2.Process.Pid, Mount: "/disk_a", JobID: "j1", NeededBytes: 5 << 30},
		"t3": {PID: cmd3.Process.Pid, Mount: "/disk_b", JobID: "j2", NeededBytes: 5 << 30},
	})
	waitForState(t, cmd1.Process.Pid, true)
	waitForState(t, cmd2.Process.Pid, true)
	waitForState(t, cmd3.Process.Pid, true)

	// Disk B has plenty of space; disk A is still tight.
	fakeFree := func(mount string) (uint64, error) {
		switch mount {
		case "/disk_a":
			return 1 << 30, nil // 1 GiB free, below 5 GiB needed
		case "/disk_b":
			return 100 << 30, nil // 100 GiB free
		}
		return 0, fmt.Errorf("unknown mount %q", mount)
	}

	result := fs.ThawTasks(fs.FrozenTaskIDs(), fakeFree)

	// t3 (on /disk_b) should be thawed; t1 and t2 (on /disk_a) blocked.
	if len(result.Thawed) != 1 || result.Thawed[0] != "t3" {
		t.Errorf("expected only t3 thawed, got %v", result.Thawed)
	}
	if len(result.Blocked) != 2 {
		t.Errorf("expected 2 blocked, got %d (%v)", len(result.Blocked), result.Blocked)
	}
	for _, tid := range []string{"t1", "t2"} {
		br, ok := result.Blocked[tid]
		if !ok {
			t.Errorf("%s missing from Blocked", tid)
			continue
		}
		if br.Mount != "/disk_a" || br.FreeBytes != 1<<30 || br.Threshold != 5<<30 {
			t.Errorf("%s BlockReason mismatch: %+v", tid, br)
		}
	}
	waitForState(t, cmd3.Process.Pid, false)
	waitForState(t, cmd1.Process.Pid, true) // still frozen
	waitForState(t, cmd2.Process.Pid, true)

	// Disk A recovers — second call should release the remaining two.
	fakeFreeRecovered := func(mount string) (uint64, error) {
		return 100 << 30, nil
	}
	result = fs.ThawTasks(fs.FrozenTaskIDs(), fakeFreeRecovered)
	if len(result.Thawed) != 2 || len(result.Blocked) != 0 {
		t.Fatalf("second pass: expected 2 thawed / 0 blocked, got %+v", result)
	}
	if fs.IsFrozen() {
		t.Error("FreezeState should be empty after both passes")
	}
	waitForState(t, cmd1.Process.Pid, false)
	waitForState(t, cmd2.Process.Pid, false)
}

// TestThawTasksBlockedOnUsageErr verifies that when diskUsage itself fails,
// every task on that mount stays frozen with FreeBytes=-1 sentinel — caller
// can distinguish "stat failed" from "stat OK, disk full".
func TestThawTasksBlockedOnUsageErr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	cmd := startSleeper(t)
	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "disk_low"}, map[string]FrozenTask{
		"t1": {PID: cmd.Process.Pid, Mount: "/broken", JobID: "j1", NeededBytes: 1 << 30},
	})

	failingFn := func(mount string) (uint64, error) {
		return 0, fmt.Errorf("stat failed")
	}
	result := fs.ThawTasks([]string{"t1"}, failingFn)
	if len(result.Thawed) != 0 {
		t.Errorf("expected no thawed when stat fails, got %v", result.Thawed)
	}
	br, ok := result.Blocked["t1"]
	if !ok {
		t.Fatal("t1 missing from Blocked")
	}
	if br.FreeBytes != -1 {
		t.Errorf("FreeBytes = %d, want -1 sentinel for stat failure", br.FreeBytes)
	}
	// Task itself stays in frozenTasks — caller can retry.
	if !fs.IsFrozen() {
		t.Error("FreezeState should remain frozen when all tasks blocked")
	}
}

// TestThawForceSkipsDiskCheck verifies that --force releases tasks even
// when NeededBytes is impossibly large — caller accepts the ENOSPC risk.
func TestThawForceSkipsDiskCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	cmd := startSleeper(t)
	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "manual"}, map[string]FrozenTask{
		"t1": {PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j1",
			NeededBytes: 1 << 60}, // 1 EiB — guaranteed to exceed any real disk
	})
	waitForState(t, cmd.Process.Pid, true)

	thawed := fs.ThawForce([]string{"t1"})
	if len(thawed) != 1 || thawed[0] != "t1" {
		t.Errorf("expected force-thaw of t1, got %v", thawed)
	}
	if fs.IsFrozen() {
		t.Error("FreezeState should clear after force-thaw of last task")
	}
	waitForState(t, cmd.Process.Pid, false)
}

// TestSIGKILLPenetratesSIGSTOP — `runq kill <task>` must work even on a
// frozen task. SIGTERM can remain pending while a task is SIGSTOPped, but
// Executor.Stop ultimately uses SIGKILL, which must terminate it promptly.
func TestSIGKILLPenetratesSIGSTOP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals differ on Windows")
	}
	cmd := startSleeper(t)
	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "test"},
		map[string]FrozenTask{"t1": {PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j1"}})
	waitForState(t, cmd.Process.Pid, true)

	if err := cmd.signalGroup(syscall.SIGKILL); err != nil {
		t.Fatalf("Kill -SIGKILL: %v", err)
	}
	if !cmd.waitExited(5 * time.Second) {
		t.Fatal("SIGKILL should have killed the frozen process within 5s")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

type testSleeper struct {
	Process *os.Process
	done    chan error
}

// startSleeper forks a `sleep 60` in its own process group so
// SIGSTOP/SIGCONT on -pgid behaves like real tasks. It starts exactly one
// waiter goroutine; tests and cleanup observe that channel instead of calling
// Cmd.Wait multiple times, which the race detector quite reasonably hates.
func startSleeper(t *testing.T) *testSleeper {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	s := &testSleeper{
		Process: cmd.Process,
		done:    make(chan error, 1),
	}
	go func() {
		s.done <- cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		if err := s.signalGroup(syscall.SIGKILL); err != nil {
			_ = s.Process.Kill()
		}
		_ = s.waitExited(2 * time.Second)
	})
	return s
}

func (s *testSleeper) signalGroup(sig syscall.Signal) error {
	pgid, err := syscall.Getpgid(s.Process.Pid)
	if err != nil {
		return err
	}
	return syscall.Kill(-pgid, sig)
}

func (s *testSleeper) waitExited(timeout time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitForState polls the process state and asserts whether it is currently
// stopped (T) or running (R/S/D). Times out after 5s — generous because
// SIGSTOP propagation can lag on slow CI.
func waitForState(t *testing.T, pid int, wantStopped bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	var lastStopped bool
	for time.Now().Before(deadline) {
		stopped, err := isStopped(pid)
		if err != nil {
			if os.IsPermission(err) {
				t.Skipf("process state inspection unavailable in this sandbox: %v", err)
			}
			lastErr = err
		}
		lastStopped = stopped
		if err == nil && stopped == wantStopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped, err := isStopped(pid)
	if err != nil && os.IsPermission(err) {
		t.Skipf("process state inspection unavailable in this sandbox: %v", err)
	}
	if err == nil {
		lastStopped = stopped
	}
	t.Errorf("pid %d: wantStopped=%v after 5s, actual=%v err=%v", pid, wantStopped, lastStopped, lastErr)
}

// isStopped inspects the OS to determine whether the process is in stopped
// (T) state. Uses /proc on Linux and `ps -o state=` on macOS/BSD.
func isStopped(pid int) (bool, error) {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false, err
		}
		// /proc/<pid>/stat fields: pid (comm) state ppid ...
		// "comm" may contain spaces, so find the closing paren first.
		s := string(data)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 >= len(s) {
			return false, nil
		}
		return s[idx+2] == 'T', nil
	default:
		// macOS / BSD: ps reports state in column 1, e.g. "T", "S", "R+".
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false, err
		}
		return strings.HasPrefix(strings.TrimSpace(string(out)), "T"), nil
	}
}
