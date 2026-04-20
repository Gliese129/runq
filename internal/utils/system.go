package utils

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
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
