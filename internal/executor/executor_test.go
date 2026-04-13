package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartSuccess(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	e := New()
	result, err := e.Start(context.Background(), RunSpec{
		TaskID:     "t1",
		Command:    `echo "hello runq"`,
		WorkingDir: dir,
		LogPath:    logPath,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.PID == 0 {
		t.Error("expected non-zero PID")
	}

	// Verify log file content
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if !strings.Contains(string(data), "hello runq") {
		t.Errorf("log should contain 'hello runq', got %q", string(data))
	}
}

func TestStartFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	e := New()
	result, err := e.Start(context.Background(), RunSpec{
		TaskID:     "t2",
		Command:    "exit 42",
		WorkingDir: dir,
		LogPath:    logPath,
	})
	if err != nil {
		t.Fatalf("Start returned error (should only set ExitCode): %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestStop(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	e := New()
	done := make(chan Result, 1)

	go func() {
		r, _ := e.Start(context.Background(), RunSpec{
			TaskID:     "t3",
			Command:    "sleep 60",
			WorkingDir: dir,
			LogPath:    logPath,
		})
		done <- r
	}()

	// Give the process time to start
	time.Sleep(200 * time.Millisecond)

	e.Stop("t3")

	select {
	case r := <-done:
		if r.ExitCode == 0 {
			t.Error("expected non-zero exit code after Stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process to stop")
	}
}

func TestStopNoop(t *testing.T) {
	e := New()
	// Should not panic
	e.Stop("nonexistent")
}

func TestBuildEnv(t *testing.T) {
	env := buildEnv([]int{0, 2, 5}, map[string]string{"WANDB_PROJECT": "test"})

	var hasCuda, hasWandb bool
	for _, e := range env {
		if e == "CUDA_VISIBLE_DEVICES=0,2,5" {
			hasCuda = true
		}
		if e == "WANDB_PROJECT=test" {
			hasWandb = true
		}
	}
	if !hasCuda {
		t.Error("missing CUDA_VISIBLE_DEVICES=0,2,5")
	}
	if !hasWandb {
		t.Error("missing WANDB_PROJECT=test")
	}
}

func TestBuildEnvNoGPU(t *testing.T) {
	env := buildEnv(nil, nil)
	for _, e := range env {
		if strings.HasPrefix(e, "CUDA_VISIBLE_DEVICES=") {
			t.Error("should not set CUDA_VISIBLE_DEVICES when no GPUs specified")
		}
	}
}

func TestCUDAVisibleInProcess(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "cuda.log")

	e := New()
	_, err := e.Start(context.Background(), RunSpec{
		TaskID:     "t-cuda",
		Command:    `echo $CUDA_VISIBLE_DEVICES`,
		WorkingDir: dir,
		GPUs:       []int{1, 3},
		LogPath:    logPath,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	if strings.TrimSpace(string(data)) != "1,3" {
		t.Errorf("expected CUDA_VISIBLE_DEVICES=1,3 in output, got %q", string(data))
	}
}
