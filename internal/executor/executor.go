package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/utils"
)

// RunSpec contains everything needed to start a task process.
type RunSpec struct {
	TaskID     string
	Command    string // full shell command, passed to "sh -c"
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
	startTime, err := utils.ReadProcessStartTime(pid)
	if err != nil {
		// Non-fatal: /proc may not exist (e.g. macOS). Log and continue.
		startTime = time.Time{}
	}

	err = cmd.Wait()

	// Kill the entire process group to clean up child processes (e.g. DataLoader
	// workers) that may still be holding GPU memory.
	killProcessGroup(pid)

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

// killProcessGroup sends SIGKILL to the entire process group of the given PID.
// Ensures child processes (DataLoader workers, etc.) don't linger holding GPU memory.
func killProcessGroup(pid int) {
	pgid, err := syscall.Getpgid(pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}
