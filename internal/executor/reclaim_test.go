package executor

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// TestReclaimWarnsOnStopped verifies the WARN emitted when reclaim
// reattaches a SIGSTOPped task. Daemon crash + restart while tasks were
// frozen leaves them stopped at OS level but freeze state is in-memory,
// so the new daemon has no record. Operators need a visible signal —
// this is that signal.
func TestReclaimWarnsOnStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}

	// Spawn a sleeper in its own pgroup and SIGSTOP it — emulating a task
	// that was frozen when the previous daemon died.
	cmd := exec.Command("sh", "-c", "sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
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
	// Wait until the state actually flips to T — SIGSTOP delivery can lag.
	deadline := time.Now().Add(2 * time.Second)
	var stopped bool
	for time.Now().Before(deadline) {
		s, err := utils.ReadProcessState(cmd.Process.Pid)
		if err != nil {
			if os.IsPermission(err) {
				t.Skipf("process state inspection unavailable in this sandbox: %v", err)
			}
		}
		if len(s) > 0 && s[0] == 'T' {
			stopped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("process state did not become T; cannot verify stopped-state reclaim warning")
	}

	// Seed a DB row claiming this PID is a running task. Reclaim will see
	// it, signal-0 reports alive, and the new state-check should fire WARN.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	// Project + Job foreign keys.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO projects (name, config_json) VALUES ('p', '{}')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "p", ConfigJSON: "{}",
		Status: "running", TotalTasks: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// StartTime left as 0 — ReclaimTask passes time.Time{} when row.StartTime
	// is zero, which makes IsProcessAlive skip the PID-reuse check entirely.
	// Test stays portable (no /proc dependency for setting startTime).
	if err := st.InsertTask(ctx, &store.TaskRow{
		ID: "t1", JobID: "j1", ProjectName: "p",
		Command: "sleep 60", ParamsJSON: "{}", GPUsNeeded: 1,
		Status: "running", PID: cmd.Process.Pid, EnqueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &Reclaimer{Store: st, Exec: New(), Logger: logger}

	alive, err := r.Reclaim(context.Background())
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if len(alive) != 1 {
		t.Errorf("expected 1 reattached task, got %d", len(alive))
	}

	out := buf.String()
	if !strings.Contains(out, "reclaimed task is in stopped state") {
		t.Errorf("expected stopped-state WARN in logs, got:\n%s", out)
	}
	if !strings.Contains(out, "t1") {
		t.Errorf("expected task id 't1' in WARN, got:\n%s", out)
	}
}
