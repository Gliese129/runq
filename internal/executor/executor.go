package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RunSpec contains everything needed to start a task process.
type RunSpec struct {
	TaskID     string
	Command    string            // full shell command, passed to "sh -c"
	WorkingDir string
	Env        map[string]string // extra env vars (merged with os.Environ)
	GPUs       []int             // GPU indices → CUDA_VISIBLE_DEVICES
	LogPath    string            // stdout+stderr are redirected here
}

// Result is returned after a process exits.
type Result struct {
	ExitCode  int
	PID       int
	StartTime time.Time // absolute process start time, for reclaim validation
}

// Executor manages running task processes and supports kill via context cancellation.
type Executor struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc // taskID → cancel
}

// New creates an Executor.
func New() *Executor {
	return &Executor{
		cancels: make(map[string]context.CancelFunc),
	}
}

// Start launches a task process and blocks until it exits or the context is cancelled.
// The caller should run this in a goroutine.
func (e *Executor) Start(parentCtx context.Context, spec RunSpec) (Result, error) {
	ctx, cancel := context.WithCancel(parentCtx)

	e.mu.Lock()
	e.cancels[spec.TaskID] = cancel
	e.mu.Unlock()

	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.cancels, spec.TaskID)
		e.mu.Unlock()
	}()

	cmd := exec.CommandContext(ctx, "sh", "-c", spec.Command)
	cmd.Dir = spec.WorkingDir
	cmd.Env = buildEnv(spec.GPUs, spec.Env)

	// Redirect stdout+stderr to log file.
	logFile, err := os.Create(spec.LogPath)
	if err != nil {
		return Result{}, fmt.Errorf("create log file %q: %w", spec.LogPath, err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start command: %w", err)
	}

	pid := cmd.Process.Pid
	startTime, err := readStartTime(pid)
	if err != nil {
		// Non-fatal: /proc may not exist (e.g. macOS). Log and continue.
		startTime = time.Time{}
	}

	err = cmd.Wait()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return Result{}, fmt.Errorf("wait for process: %w", err)
		}
	}

	return Result{
		ExitCode:  exitCode,
		PID:       pid,
		StartTime: startTime,
	}, nil
}

// Stop terminates the process for the given taskID by cancelling its context.
// No-op if the task is not running.
func (e *Executor) Stop(taskID string) {
	e.mu.Lock()
	cancel, ok := e.cancels[taskID]
	e.mu.Unlock()

	if ok {
		cancel()
	}
}

// buildEnv merges the current environment with CUDA_VISIBLE_DEVICES and extra vars.
func buildEnv(gpus []int, extra map[string]string) []string {
	env := os.Environ()

	if len(gpus) > 0 {
		parts := make([]string, len(gpus))
		for i, g := range gpus {
			parts[i] = strconv.Itoa(g)
		}
		env = append(env, "CUDA_VISIBLE_DEVICES="+strings.Join(parts, ","))
	}

	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// clkTick is the kernel clock tick rate (USER_HZ).
// 100 is the standard value on x86_64 Linux. If this ever needs to be
// dynamic, read it via sysconf(_SC_CLK_TCK) or /proc/self/auxv.
const clkTick = 100

// readStartTime reads the process start time from /proc/<pid>/stat and
// converts it to an absolute time.Time using boot time from /proc/stat.
func readStartTime(pid int) (time.Time, error) {
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

	bootTime, err := getBootTime()
	if err != nil {
		return time.Time{}, fmt.Errorf("read boot time: %w", err)
	}

	seconds := tick / clkTick
	nanoRemainder := (tick % clkTick) * (1e9 / clkTick)
	return time.Unix(bootTime+seconds, nanoRemainder), nil
}

// getBootTime reads the system boot time (seconds since epoch) from /proc/stat.
func getBootTime() (int64, error) {
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
	return 0, fmt.Errorf("btime not found in /proc/stat")
}
