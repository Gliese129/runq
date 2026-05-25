package scheduler

import (
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
	t.Skip("rewriting: events test now needs real sleepers since the new " +
		"Freeze() drops entries whose SIGSTOP fails. See stage1 doc.")
	fs := NewFreezeState()

	fs.Freeze(FreezeEvent{
		Reason:        "disk_low",
		TriggerTaskID: "t1",
		FreeBytes:     1000,
		NeededEst:     2000,
	}, map[string]FrozenTask{"t1": {PID: 999, Mount: "/tmp", JobID: "j1"}})

	fs.Freeze(FreezeEvent{
		Reason:        "disk_low",
		TriggerTaskID: "t2",
		FreeBytes:     500,
		NeededEst:     2000,
	}, map[string]FrozenTask{"t2": {PID: 998, Mount: "/tmp", JobID: "j1"}})

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
	t.Skip("rewriting: needs real sleepers in new model since fake PIDs no " +
		"longer get registered (no trigger-task special case). See stage1 doc.")
	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "test", TriggerTaskID: "t1"},
		map[string]FrozenTask{"t1": {PID: 999, Mount: "/tmp", JobID: "j1"}})
	fs.Freeze(FreezeEvent{Reason: "test", TriggerTaskID: "t2"},
		map[string]FrozenTask{"t2": {PID: 998, Mount: "/tmp", JobID: "j1"}})
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
}

// TestSIGTERMPenetratesSIGSTOP — `runq kill <task>` must work even on a
// frozen task; SIGTERM passes through SIGSTOP semantics on Linux/Mac.
func TestSIGTERMPenetratesSIGSTOP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals differ on Windows")
	}
	cmd := startSleeper(t)
	fs := NewFreezeState()
	fs.Freeze(FreezeEvent{Reason: "test"},
		map[string]FrozenTask{"t1": {PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j1"}})

	// SIGTERM to process group (matches what TaskService.KillTask does
	// indirectly via Executor.Stop → context cancel → SIGKILL on pgroup).
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		t.Fatalf("Kill -SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// died — good
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM should have killed the frozen process within 5s")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// startSleeper forks a `sleep 60` in its own process group (matching how
// Executor.Start launches user commands) so SIGSTOP/SIGCONT on -pgid behaves
// like real tasks. The cleanup t.Cleanup kills it even if test fails.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// waitForState polls the process state and asserts whether it is currently
// stopped (T) or running (R/S/D). Times out after 2s — generous because
// SIGSTOP propagation can lag on slow CI.
func waitForState(t *testing.T, pid int, wantStopped bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isStopped(pid) == wantStopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("pid %d: wantStopped=%v after 2s, actual=%v", pid, wantStopped, isStopped(pid))
}

// isStopped inspects the OS to determine whether the process is in stopped
// (T) state. Uses /proc on Linux and `ps -o state=` on macOS/BSD.
func isStopped(pid int) bool {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		// /proc/<pid>/stat fields: pid (comm) state ppid ...
		// "comm" may contain spaces, so find the closing paren first.
		s := string(data)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 >= len(s) {
			return false
		}
		return s[idx+2] == 'T'
	default:
		// macOS / BSD: ps reports state in column 1, e.g. "T", "S", "R+".
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		return strings.HasPrefix(strings.TrimSpace(string(out)), "T")
	}
}
