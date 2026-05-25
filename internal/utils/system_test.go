package utils

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestReadProcessStateRunning spawns a `sleep 60` and verifies its state
// reads back as one of the "alive" letters (S, R, I depending on OS).
// Both Linux (/proc) and macOS (ps) paths exercise this.
func TestReadProcessStateRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ReadProcessState relies on /proc or ps; not implemented for Windows")
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

	// Give the process a beat to actually be sleeping.
	time.Sleep(50 * time.Millisecond)

	state, err := ReadProcessState(cmd.Process.Pid)
	if err != nil {
		if os.IsPermission(err) {
			t.Skipf("process state inspection unavailable in this sandbox: %v", err)
		}
		t.Fatalf("ReadProcessState: %v", err)
	}
	// macOS reports states with modifiers (e.g. "Ss", "S+"); we only check
	// the first character.
	switch state[0] {
	case 'S', 'R', 'I', 'D':
		// expected alive states
	case 'T':
		t.Errorf("unexpected stopped state for newly-started sleep: %q", state)
	default:
		t.Errorf("unexpected state %q (want one of S/R/I/D)", state)
	}
}

// TestReadProcessStateStopped SIGSTOPs a sleeper and verifies the state
// flips to 'T'. This is the path daemon reclaim cares about — detecting
// pre-restart frozen tasks.
func TestReadProcessStateStopped(t *testing.T) {
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

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if err := syscall.Kill(-pgid, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}

	// Poll for up to 2s — SIGSTOP propagation can lag on slow CI.
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		state, err := ReadProcessState(cmd.Process.Pid)
		if err != nil {
			if os.IsPermission(err) {
				t.Skipf("process state inspection unavailable in this sandbox: %v", err)
			}
			lastErr = err
		}
		if err == nil && len(state) > 0 && state[0] == 'T' {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, err := ReadProcessState(cmd.Process.Pid)
	if err != nil && os.IsPermission(err) {
		t.Skipf("process state inspection unavailable in this sandbox: %v", err)
	}
	t.Errorf("state did not become 'T' within 2s, last read: %q err=%v", state, lastErr)
}

// TestReadProcessStateDead asserts that a stale PID returns an error
// rather than a stale state. Important: reclaim should treat error as
// "process is gone" and not WARN as if it were SIGSTOPped.
func TestReadProcessStateDead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ReadProcessState not implemented for Windows")
	}
	// Use PID 0 which never represents a real process.
	if _, err := ReadProcessState(0); err == nil {
		t.Error("ReadProcessState(0) should return error, got nil")
	}
}

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
	// Wait for actual state transition.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := ReadProcessState(cmd.Process.Pid); len(s) > 0 && s[0] == 'T' {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

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
