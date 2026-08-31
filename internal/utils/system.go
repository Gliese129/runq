package utils

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const clkTick = 100

// GetBootTime reads the system boot time (seconds since epoch) from /proc/stat.
func GetBootTime() (int64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "btime ") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("malformed btime line: %q", line)
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("boot time not found in /proc/stat")
}

// ReadProcessStartTime reads the process start time from /proc/<pid>/stat and
// converts it to an absolute time.Time using boot time from /proc/stat.
func ReadProcessStartTime(pid int) (time.Time, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, err
	}

	s := string(data)
	// Field 22 (starttime) comes after "(comm)" which may contain spaces/parens.
	// Find the last ')' to safely skip the comm field.
	lastParen := strings.LastIndex(s, ")")
	if lastParen < 0 || lastParen+2 >= len(s) {
		return time.Time{}, fmt.Errorf("invalid /proc/%d/stat format", pid)
	}

	fields := strings.Fields(s[lastParen+2:])
	if len(fields) < 20 {
		return time.Time{}, fmt.Errorf("/proc/%d/stat: not enough fields after comm", pid)
	}

	tick, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse starttime tick: %w", err)
	}

	bootTime, err := GetBootTime()
	if err != nil {
		return time.Time{}, fmt.Errorf("read boot time: %w", err)
	}

	seconds := tick / clkTick
	nanoRemainder := (tick % clkTick) * (1e9 / clkTick)
	return time.Unix(bootTime+seconds, nanoRemainder), nil
}

// IsProcessAlive checks if a process with the given PID is still running.
//
// Two checks:
//  1. signal-0 to verify the kernel sees the PID.
//  2. start-time comparison to defend against PID reuse — if the recorded
//     start time and the current one differ by more than 10s, the PID has
//     been recycled into a different process and we report it dead.
//
// The start-time check is skipped in two cases:
//   - expectedStartTime.IsZero(): caller doesn't have a start time to
//     compare (legacy DB rows pre-StartTime, fresh inserts where the
//     column wasn't populated, test setups). Treat signal-0 as
//     authoritative — caller has accepted the PID-reuse risk by passing
//     zero.
//   - ReadProcessStartTime fails: typically macOS / non-Linux where there
//     is no /proc. Same fallback: trust signal-0.
//
// Passing zero is the explicit way for client-daemon callers to say they do
// not have a reliable start-time baseline.
func IsProcessAlive(pid int, expectedStartTime time.Time) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without actually sending a signal.
	//
	// Must be syscall.Signal(0) — NOT os.Signal(nil). The latter is a nil
	// interface; the stdlib's Process.Signal does a type assertion to
	// syscall.Signal and returns "unsupported signal type" for nil
	// interfaces. The previous version of this code used os.Signal(nil)
	// and effectively made IsProcessAlive always return false, which
	// silently broke daemon reclaim of live tasks.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	// Caller opted out of PID-reuse check.
	if expectedStartTime.IsZero() {
		return true
	}
	startTime, err := ReadProcessStartTime(pid)
	if err != nil {
		// /proc not readable (macOS / BSD) — can't verify, trust signal-0.
		return true
	}
	const tolerance = 10 * time.Second
	diff := startTime.Sub(expectedStartTime)
	if diff < -tolerance || diff > tolerance {
		return false
	}
	return true
}
