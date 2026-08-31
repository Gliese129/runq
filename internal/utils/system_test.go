package utils

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestIsProcessAliveLiveProcess is a regression test for the signal-0 bug:
// a previous version of IsProcessAlive used os.Signal(nil) which always
// failed the syscall.Signal assertion inside the stdlib, making the
// function return false for ANY pid. Daemon reclaim was silently
// returning "every task is dead" on restart.
//
// This test spawns a real sleeper, then verifies IsProcessAlive returns
// true with zero-expectedStartTime (signal-0 only).
func TestIsProcessAliveLiveProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal semantics differ on Windows")
	}
	cmd := exec.Command("sh", "-c", "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	time.Sleep(50 * time.Millisecond)

	if !IsProcessAlive(cmd.Process.Pid, time.Time{}) {
		t.Error("IsProcessAlive returned false for a freshly-started sleeper")
	}
}

// TestIsProcessAliveSIGSTOPped confirms that SIGSTOP doesn't make the
// process "dead" from signal-0's perspective. Reclaim relies on this to
// reattach to tasks that were frozen when the previous daemon died.
func TestIsProcessAliveSIGSTOPped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	cmd := exec.Command("sh", "-c", "sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	pgid, _ := syscall.Getpgid(cmd.Process.Pid)
	if err := syscall.Kill(-pgid, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if !IsProcessAlive(cmd.Process.Pid, time.Time{}) {
		t.Error("IsProcessAlive returned false for SIGSTOPped process")
	}
}

// TestIsProcessAliveDead confirms that genuinely dead PIDs return false.
func TestIsProcessAliveDead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal semantics differ on Windows")
	}
	if IsProcessAlive(0, time.Time{}) {
		t.Error("IsProcessAlive(0, _) should be false")
	}
}
