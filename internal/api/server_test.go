package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupTestServer(t *testing.T) *Server {
	t.Helper()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := project.NewRegistry(st.DB())
	q := scheduler.NewQueue()
	pool := resource.NewMockAllocator(2)
	exec := executor.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	deps := Deps{
		Store:    st,
		Registry: reg,
		Queue:    q,
		Pool:     pool,
		Executor: exec,
		Logger:   logger,
		JobService: &service.JobService{
			Store: st, Queue: q, Exec: exec, Registry: reg,
		},
		TaskService: &service.TaskService{
			Store: st, Queue: q, Exec: exec, Scheduler: nil,
		},
	}

	return NewServer(deps, "", "")
}

// doRequest sends a test HTTP request through the Gin router.
func doRequest(s *Server, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)
	return w
}

func TestProjectCRUD(t *testing.T) {
	s := setupTestServer(t)
	workDir := t.TempDir()

	// Add
	cfg := project.Config{
		ProjectName: "resnet50",
		WorkingDir:  workDir,
		CmdTemplate: "python train.py {{args}}",
	}
	w := doRequest(s, "POST", "/api/projects", cfg)
	if w.Code != http.StatusCreated {
		t.Fatalf("Add: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// project.yaml must exist on disk
	if _, err := os.Stat(workDir + "/project.yaml"); err != nil {
		t.Fatalf("project.yaml not written: %v", err)
	}

	// List
	w = doRequest(s, "GET", "/api/projects", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("List: expected 200, got %d", w.Code)
	}
	var configs []project.Config
	json.NewDecoder(w.Body).Decode(&configs)
	if len(configs) != 1 || configs[0].ProjectName != "resnet50" {
		t.Errorf("List: unexpected result: %+v", configs)
	}

	// Get
	w = doRequest(s, "GET", "/api/projects/resnet50", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("Get: expected 200, got %d", w.Code)
	}

	// Get not found
	w = doRequest(s, "GET", "/api/projects/nonexistent", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("Get missing: expected 404, got %d", w.Code)
	}

	// Add with bad dir must fail
	badCfg := project.Config{
		ProjectName: "bad",
		WorkingDir:  "/this/does/not/exist",
		CmdTemplate: "echo",
	}
	w = doRequest(s, "POST", "/api/projects", badCfg)
	if w.Code == http.StatusCreated {
		t.Error("Add with non-existent dir should fail")
	}

	// Delete endpoint removed in Phase 2F — use clean command instead.
}

func TestTaskListDefault(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	// Seed project + job + tasks in DB (handleTaskList now reads from Store).
	st := s.deps.Store
	st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('test', '{}')`)
	st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "test", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 2, CreatedAt: time.Now(),
	})
	for _, id := range []string{"t1", "t2"} {
		st.InsertTask(ctx, &store.TaskRow{
			ID: id, JobID: "j1", ProjectName: "test",
			Command: "echo hi", ParamsJSON: "{}", GPUsNeeded: 1,
			Status: "pending", EnqueuedAt: time.Now(),
		})
	}

	w := doRequest(s, "GET", "/api/tasks", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var tasks []store.TaskRow
	json.NewDecoder(w.Body).Decode(&tasks)
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestTaskListStatusAll(t *testing.T) {
	s := setupTestServer(t)
	ctx := context.Background()

	st := s.deps.Store
	st.DB().Exec(`INSERT INTO projects (name, config_json) VALUES ('test', '{}')`)
	st.InsertJob(ctx, &store.JobRow{
		ID: "j1", ProjectName: "test", ConfigJSON: "{}",
		Status: "pending", TotalTasks: 2, CreatedAt: time.Now(),
	})
	for _, tt := range []struct {
		id     string
		status string
	}{
		{id: "t1", status: "pending"},
		{id: "t2", status: "success"},
	} {
		st.InsertTask(ctx, &store.TaskRow{
			ID: tt.id, JobID: "j1", ProjectName: "test",
			Command: "echo hi", ParamsJSON: "{}", GPUsNeeded: 1,
			Status: tt.status, EnqueuedAt: time.Now(),
		})
	}

	w := doRequest(s, "GET", "/api/tasks", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("default list: expected 200, got %d", w.Code)
	}
	var active []store.TaskRow
	json.NewDecoder(w.Body).Decode(&active)
	if len(active) != 1 || active[0].ID != "t1" {
		t.Fatalf("default list: expected only active task t1, got %+v", active)
	}

	w = doRequest(s, "GET", "/api/tasks?status=all", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=all: expected 200, got %d", w.Code)
	}
	var all []store.TaskRow
	json.NewDecoder(w.Body).Decode(&all)
	if len(all) != 2 {
		t.Fatalf("status=all: expected 2 tasks, got %+v", all)
	}

	w = doRequest(s, "GET", "/api/tasks?status=all&job=j1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=all&job: expected 200, got %d", w.Code)
	}
	all = nil
	json.NewDecoder(w.Body).Decode(&all)
	if len(all) != 2 {
		t.Fatalf("status=all&job: expected 2 tasks, got %+v", all)
	}
}

func TestGPUStatus(t *testing.T) {
	s := setupTestServer(t)

	w := doRequest(s, "GET", "/api/gpu", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var gpus []resource.GPUState
	json.NewDecoder(w.Body).Decode(&gpus)
	if len(gpus) != 2 {
		t.Errorf("expected 2 GPUs, got %d", len(gpus))
	}
}

func TestStatus(t *testing.T) {
	s := setupTestServer(t)

	w := doRequest(s, "GET", "/api/status", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var status map[string]any
	json.NewDecoder(w.Body).Decode(&status)
	if status["gpus_free"] != float64(2) {
		t.Errorf("expected 2 free GPUs, got %v", status["gpus_free"])
	}
}

// setupTestServerWithFreeze adds a wired FreezeState — used by thaw tests
// since setupTestServer leaves Deps.Freeze nil (verifies 503 path).
func setupTestServerWithFreeze(t *testing.T) (*Server, *scheduler.FreezeState) {
	t.Helper()
	s := setupTestServer(t)
	fs := scheduler.NewFreezeState()
	s.deps.Freeze = fs
	return s, fs
}

// TestThawUnfrozen: hitting /api/thaw on an idle daemon must succeed
// (idempotent) — users may script this defensively.
//
// Returns ThawResponse with empty Thawed and Blocked.
func TestThawUnfrozen(t *testing.T) {
	s, _ := setupTestServerWithFreeze(t)
	w := doRequest(s, "POST", "/api/thaw", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp backend.ThawResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Thawed) != 0 {
		t.Errorf("Thawed should be empty on idle daemon, got %v", resp.Thawed)
	}
	if len(resp.Blocked) != 0 {
		t.Errorf("Blocked should be empty on idle daemon, got %v", resp.Blocked)
	}
}

// startTestSleeper forks `sleep 60` in its own pgroup so Freeze can SIGSTOP
// it. Used by the freeze/thaw e2e tests below.
func startTestSleeper(t *testing.T) *exec.Cmd {
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

// TestThawFrozenBlocked: checked thaw with an impossibly-large NeededBytes
// must return the task in Blocked, not Thawed. Verifies the
// ThawResponse{Thawed, Blocked} shape end-to-end and confirms the daemon
// preserves the per-task threshold from FrozenTask.NeededBytes.
func TestThawFrozenBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	s, fs := setupTestServerWithFreeze(t)
	cmd := startTestSleeper(t)

	fs.Freeze(
		scheduler.FreezeEvent{Reason: "manual", TriggerTaskID: "t1"},
		map[string]scheduler.FrozenTask{
			"t1": {
				PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j1",
				NeededBytes: 1 << 60, // 1 EiB — guaranteed to exceed disk
			},
		},
	)
	if !fs.IsFrozen() {
		t.Fatal("setup: freeze didn't take")
	}

	w := doRequest(s, "POST", "/api/thaw", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp backend.ThawResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Thawed) != 0 {
		t.Errorf("Thawed should be empty when threshold exceeds disk: got %v", resp.Thawed)
	}
	br, ok := resp.Blocked["t1"]
	if !ok {
		t.Fatalf("t1 missing from Blocked: %+v", resp.Blocked)
	}
	if br.Mount != "/tmp" {
		t.Errorf("Blocked.Mount = %q, want /tmp", br.Mount)
	}
	if br.Threshold != 1<<60 {
		t.Errorf("Blocked.Threshold = %d, want 1<<60 (per-task NeededBytes)", br.Threshold)
	}
	if !fs.IsFrozen() {
		t.Error("FreezeState should remain frozen — nothing thawed")
	}
}

// TestThawFrozenForce: ?force=true releases tasks regardless of disk state.
// Verifies the wiring between the query param and FreezeState.ThawForce.
func TestThawFrozenForce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}
	s, fs := setupTestServerWithFreeze(t)
	cmd := startTestSleeper(t)

	fs.Freeze(
		scheduler.FreezeEvent{Reason: "manual", TriggerTaskID: "t1"},
		map[string]scheduler.FrozenTask{
			"t1": {
				PID: cmd.Process.Pid, Mount: "/tmp", JobID: "j1",
				NeededBytes: 1 << 60, // same impossible threshold
			},
		},
	)

	w := doRequest(s, "POST", "/api/thaw?force=true", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp backend.ThawResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Thawed) != 1 || resp.Thawed[0] != "t1" {
		t.Errorf("Thawed = %v, want [t1]", resp.Thawed)
	}
	if len(resp.Blocked) != 0 {
		t.Errorf("Blocked should be empty under --force: %+v", resp.Blocked)
	}
	if fs.IsFrozen() {
		t.Error("FreezeState should drain after force-thaw of last task")
	}
}

// TestThawWithoutFreezeState: daemons built without freeze wiring (test
// shortcuts, future modes) should 503 rather than panic on nil deref.
func TestThawWithoutFreezeState(t *testing.T) {
	s := setupTestServer(t) // no Freeze in Deps
	w := doRequest(s, "POST", "/api/thaw", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when Freeze not wired, got %d", w.Code)
	}
}
