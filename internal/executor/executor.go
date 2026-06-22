package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/hpccore"

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
	TaskDir    string            // per-task workspace dir (for activity.tsv)
	OnStart    func(Result)      // called after cmd.Start, before waiting
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

	command := spec.Command
	// Ambient env file (RUNQ_ENV_FILE): sourced at task start, then the
	// explicit env is re-exported ON TOP so explicit config always wins
	// (precedence: .env < project/override env). runq never reads the
	// file's values — secrets stay out of the DB, logs, and UIs.
	if envFile := spec.Env["RUNQ_ENV_FILE"]; envFile != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "if [ -f %s ]; then set -a; . %s; set +a; fi\n", hpccore.ShellQuote(envFile), hpccore.ShellQuote(envFile))
		for k, v := range spec.Env {
			fmt.Fprintf(&b, "export %s=%s\n", k, hpccore.ShellQuote(v))
		}
		b.WriteString(command)
		command = b.String()
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = spec.WorkingDir
	cmd.Env = buildEnv(spec.GPUs, spec.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Redirect stdout+stderr to log file.
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create log directory for %q: %w", spec.LogPath, err)
	}
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
	if spec.OnStart != nil {
		spec.OnStart(Result{PID: pid, StartTime: startTime})
	}

	go recordActivity(ctx, spec.LogPath, spec.TaskDir)

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

// recordActivity samples log file size and incrementally counts lines every 60s,
// appending 3-column rows {ts, cumulative_bytes, cumulative_lines} to activity.tsv.
// Exits when ctx is cancelled (task finishes).
func recordActivity(ctx context.Context, logPath, taskDir string) {
	if logPath == "" || taskDir == "" {
		return
	}
	activityPath := filepath.Join(taskDir, "activity.tsv")
	var prevBytes int64
	var cumLines int64
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(logPath)
			if err != nil {
				continue
			}
			curBytes := info.Size()

			// Count newlines in the delta bytes since last sample.
			if delta := curBytes - prevBytes; delta > 0 {
				cumLines += countNewlines(logPath, prevBytes, delta)
			}
			prevBytes = curBytes

			line := fmt.Sprintf("%d\t%d\t%d\n", time.Now().Unix(), curBytes, cumLines)
			f, err := os.OpenFile(activityPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				continue
			}
			_, _ = f.WriteString(line)
			f.Close()
		}
	}
}

// countNewlines reads [offset, offset+count) from path and returns the number
// of '\n' bytes. On any error it returns 0 (best-effort; the next tick catches up).
func countNewlines(path string, offset, count int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0
	}

	var total int64
	buf := make([]byte, 64*1024)
	remaining := count
	for remaining > 0 {
		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, err := f.Read(buf[:toRead])
		if n > 0 {
			total += int64(bytes.Count(buf[:n], []byte{'\n'}))
			remaining -= int64(n)
		}
		if err != nil {
			break
		}
	}
	return total
}
