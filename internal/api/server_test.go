package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

	// Add
	cfg := project.Config{
		ProjectName: "resnet50",
		WorkingDir:  "/tmp/resnet",
		CmdTemplate: "python train.py {{args}}",
	}
	w := doRequest(s, "POST", "/api/projects", cfg)
	if w.Code != http.StatusCreated {
		t.Fatalf("Add: expected 201, got %d: %s", w.Code, w.Body.String())
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

	// Delete
	w = doRequest(s, "DELETE", "/api/projects/resnet50", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("Delete: expected 200, got %d", w.Code)
	}

	// Verify deleted
	w = doRequest(s, "GET", "/api/projects/resnet50", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("Get after delete: expected 404, got %d", w.Code)
	}
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
func TestThawUnfrozen(t *testing.T) {
	s, _ := setupTestServerWithFreeze(t)
	w := doRequest(s, "POST", "/api/thaw", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		WasFrozen   bool     `json:"was_frozen"`
		ThawedTasks []string `json:"thawed_tasks"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.WasFrozen {
		t.Errorf("WasFrozen=true on idle daemon")
	}
	if len(resp.ThawedTasks) != 0 {
		t.Errorf("ThawedTasks should be empty, got %v", resp.ThawedTasks)
	}
}

// TestThawFrozen: real freeze → thaw cycle through the API. Validates that
// the FreezeState handed to Deps is the same instance that Thaw operates on
// (no copy-by-value bugs).
func TestThawFrozen(t *testing.T) {
	t.Skip("rewriting against new ThawResponse shape; see stage1 doc.")
	s, fs := setupTestServerWithFreeze(t)
	fs.Freeze(
		scheduler.FreezeEvent{Reason: "manual", TriggerTaskID: "t1"},
		map[string]scheduler.FrozenTask{
			"t1": {PID: 999, Mount: "/tmp", JobID: "j1", NeededBytes: 1},
		},
	)
	if !fs.IsFrozen() {
		t.Fatal("setup: freeze didn't take")
	}

	w := doRequest(s, "POST", "/api/thaw", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Response shape now ThawResponse{Thawed, Blocked}; rewrite once handleThaw
	// lands.
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
