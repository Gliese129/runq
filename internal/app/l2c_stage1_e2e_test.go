package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

type l2cStage1Harness struct {
	ctx      context.Context
	workDir  string
	store    *store.Store
	registry *project.Registry
	queue    *scheduler.Queue
	pool     *resource.MockAllocator
	exec     *executor.Executor
	freeze   *scheduler.FreezeState
	sched    *scheduler.Scheduler
	server   *api.Server
	jobSvc   *service.JobService
	taskSvc  *service.TaskService
}

func newL2CStage1Harness(t *testing.T, gpuCount int, cmdTemplate string) *l2cStage1Harness {
	t.Helper()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	workDir := t.TempDir()
	reg := project.NewRegistry(st.DB())
	if err := reg.Add(project.Config{
		ProjectName: "l2c-e2e",
		WorkingDir:  workDir,
		CmdTemplate: cmdTemplate,
		Defaults:    project.Defaults{GPUsPerTask: 1},
	}); err != nil {
		t.Fatalf("register project: %v", err)
	}

	q := scheduler.NewQueue()
	pool := resource.NewMockAllocator(gpuCount)
	exec := executor.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	freeze := scheduler.NewFreezeState()

	cfg := scheduler.DefaultConfig()
	cfg.TickInterval = 10 * time.Millisecond
	cfg.GPURefreshInterval = 0
	cfg.Disk.AutoThawCheckInterval = time.Hour

	sched := scheduler.New(cfg, q, pool, exec, st, logger, nil, "/tmp/runq-l2c-e2e.sock", freeze)
	t.Cleanup(func() { sched.Shutdown() })

	jobSvc := &service.JobService{
		Store: st, Queue: q, Scheduler: sched, Exec: exec, Registry: reg, Pool: pool,
	}
	taskSvc := &service.TaskService{
		Store: st, Queue: q, Exec: exec, Scheduler: sched,
	}

	server := api.NewServer(api.Deps{
		Store:       st,
		Registry:    reg,
		Scheduler:   sched,
		Queue:       q,
		Pool:        pool,
		Executor:    exec,
		Logger:      logger,
		JobService:  jobSvc,
		TaskService: taskSvc,
		Freeze:      freeze,
	}, "", "")

	return &l2cStage1Harness{
		ctx:      context.Background(),
		workDir:  workDir,
		store:    st,
		registry: reg,
		queue:    q,
		pool:     pool,
		exec:     exec,
		freeze:   freeze,
		sched:    sched,
		server:   server,
		jobSvc:   jobSvc,
		taskSvc:  taskSvc,
	}
}

func (h *l2cStage1Harness) submitSweepJob(t *testing.T, values []any) (string, []store.TaskRow) {
	t.Helper()
	jobID, n, err := h.jobSvc.SubmitJob(h.ctx, job.JobConfig{
		Project: "l2c-e2e",
		Sweep: []job.SweepBlock{
			{
				Method: "grid",
				Parameters: map[string]job.ParameterSpec{
					"x": {Values: values},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if n != len(values) {
		t.Fatalf("SubmitJob task count = %d, want %d", n, len(values))
	}
	rows, err := h.store.ListTasks(h.ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(rows) != len(values) {
		t.Fatalf("stored task count = %d, want %d", len(rows), len(values))
	}
	return jobID, rows
}

func (h *l2cStage1Harness) addPendingTaskToJob(t *testing.T, jobID string) string {
	t.Helper()
	taskID := service.GenerateID()
	taskDir := filepath.Join(h.workDir, ".runq", taskID)
	ckptDir := filepath.Join(taskDir, "checkpoints")
	if err := os.MkdirAll(ckptDir, 0o755); err != nil {
		t.Fatalf("mkdir task workspace: %v", err)
	}
	task := &scheduler.Task{
		ID:            taskID,
		JobID:         jobID,
		ProjectName:   "l2c-e2e",
		Command:       "sleep 30",
		GPUsNeeded:    1,
		MaxRetry:      1,
		LogPath:       filepath.Join(h.workDir, "logs", taskID+".log"),
		WorkingDir:    h.workDir,
		UID:           os.Getuid(),
		TaskDir:       taskDir,
		CheckpointDir: ckptDir,
	}
	if err := h.store.InsertTask(h.ctx, &store.TaskRow{
		ID: task.ID, JobID: task.JobID, ProjectName: task.ProjectName,
		Command: task.Command, ParamsJSON: "{}", GPUsNeeded: task.GPUsNeeded,
		Status: "pending", MaxRetry: task.MaxRetry, LogPath: task.LogPath,
		WorkingDir: task.WorkingDir, UID: task.UID, EnqueuedAt: time.Now(),
		TaskDir: task.TaskDir,
	}); err != nil {
		t.Fatalf("insert sibling task: %v", err)
	}
	h.queue.Push(task)
	return taskID
}

func (h *l2cStage1Harness) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.server.Router().ServeHTTP(w, req)
	return w
}

func decodeThawResponse(t *testing.T, w *httptest.ResponseRecorder) api.ThawResponse {
	t.Helper()
	var resp api.ThawResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode ThawResponse: %v", err)
	}
	return resp
}

func (h *l2cStage1Harness) waitForRunningTask(t *testing.T, taskID string) *scheduler.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task := h.queue.Get(taskID)
		if task != nil && task.Status == scheduler.StatusRunning && task.PID > 0 {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := h.store.GetTask(h.ctx, taskID)
	t.Fatalf("task %s did not reach running+pid; db row=%+v", taskID, row)
	return nil
}

func (h *l2cStage1Harness) waitForDBStatus(t *testing.T, taskID, status string) store.TaskRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := h.store.GetTask(h.ctx, taskID)
		if err != nil {
			t.Fatalf("get task %s: %v", taskID, err)
		}
		if row != nil && row.Status == status {
			return *row
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := h.store.GetTask(h.ctx, taskID)
	t.Fatalf("task %s did not reach status %q; db row=%+v", taskID, status, row)
	return store.TaskRow{}
}

func waitForProcessStopped(t *testing.T, pid int, wantStopped bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, err := utils.ReadProcessState(pid)
		if err != nil && os.IsPermission(err) {
			t.Logf("skipping process-state assertion for pid %d: %v", pid, err)
			return
		}
		if err == nil && (state == "T") == wantStopped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, err := utils.ReadProcessState(pid)
	if err != nil && os.IsPermission(err) {
		t.Logf("skipping process-state assertion for pid %d: %v", pid, err)
		return
	}
	t.Fatalf("pid %d stopped=%v not observed; final state=%q err=%v", pid, wantStopped, state, err)
}

func mountForTask(t *testing.T, task *scheduler.Task) string {
	t.Helper()
	parts, err := utils.LoadMountTable()
	if err != nil {
		t.Fatalf("load mount table: %v", err)
	}
	mount := utils.MountOf(task.CheckpointDir, parts)
	if mount == "" {
		// macOS APFS firmlinks can make prefix-based mount lookup miss
		// paths under /var. disk.Usage accepts a directory path, so use
		// the checkpoint dir as a stable fallback for freeze/thaw tests.
		return task.CheckpointDir
	}
	return mount
}

func taskIDs(rows []store.TaskRow) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

func assertIDsEqualSet(t *testing.T, got []string, want []string) {
	t.Helper()
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	if fmt.Sprint(gotCopy) != fmt.Sprint(wantCopy) {
		t.Fatalf("ids = %v, want %v", gotCopy, wantCopy)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestL2CStage1E2ENormalCheckedThaw(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}

	h := newL2CStage1Harness(t, 1, "sleep 30 # {{args}}")
	h.sched.Start()

	_, rows := h.submitSweepJob(t, []any{1})
	taskID := rows[0].ID
	task := h.waitForRunningTask(t, taskID)
	mount := mountForTask(t, task)

	w := h.request(t, http.MethodPost, "/api/internal/freeze-self", api.FreezeSelfReq{
		TaskID: taskID, FreeBytes: 1 << 40, NeededEst: 1, Mount: mount,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("freeze-self expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !h.freeze.IsFrozen() {
		t.Fatal("FreezeState should be frozen after freeze-self")
	}
	waitForProcessStopped(t, task.PID, true)

	w = h.request(t, http.MethodPost, fmt.Sprintf("/api/thaw?owner=%d", os.Getuid()), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("checked thaw expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeThawResponse(t, w)
	assertIDsEqualSet(t, resp.Thawed, []string{taskID})
	if len(resp.Blocked) != 0 {
		t.Fatalf("checked thaw should not block tiny threshold: %+v", resp.Blocked)
	}
	if h.freeze.IsFrozen() {
		t.Fatal("FreezeState should drain after checked thaw")
	}
	waitForProcessStopped(t, task.PID, false)

	w = h.request(t, http.MethodPost, fmt.Sprintf("/api/thaw?owner=%d", os.Getuid()), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent thaw expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp = decodeThawResponse(t, w)
	if len(resp.Thawed) != 0 || len(resp.Blocked) != 0 {
		t.Fatalf("second thaw should be empty, got %+v", resp)
	}
}

func TestL2CStage1E2EStressExtremeFreezeThaw(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGSTOP not available on Windows")
	}

	const taskCount = 8
	values := make([]any, taskCount)
	for i := range values {
		values[i] = i
	}

	h := newL2CStage1Harness(t, taskCount+1, "sleep 30 # {{args}}")
	h.sched.Start()

	jobID, rows := h.submitSweepJob(t, values)
	ids := taskIDs(rows)

	running := make(map[string]*scheduler.Task, taskCount)
	for _, id := range ids {
		running[id] = h.waitForRunningTask(t, id)
	}
	mount := mountForTask(t, running[ids[0]])

	ckpts := make([]store.CheckpointRow, 0, taskCount)
	for i, id := range ids {
		step := int64(i + 1)
		ckpts = append(ckpts, store.CheckpointRow{
			TaskID: id, JobID: jobID,
			Path:      filepath.Join(h.workDir, ".runq", id, "checkpoints", fmt.Sprintf("ckpt-%02d.pt", i)),
			SizeBytes: int64(i+1) * 1024,
			Step:      int64Ptr(step),
			TS:        1700000000 + int64(i),
		})
	}
	if err := h.store.InsertCheckpointsBatch(h.ctx, ckpts); err != nil {
		t.Fatalf("seed checkpoints: %v", err)
	}

	w := h.request(t, http.MethodPost, "/api/internal/freeze-self", api.FreezeSelfReq{
		TaskID: "missing-task", FreeBytes: 1, NeededEst: 1, Mount: mount,
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing task freeze expected 404, got %d: %s", w.Code, w.Body.String())
	}

	for _, id := range ids {
		w = h.request(t, http.MethodPost, "/api/internal/freeze-self", api.FreezeSelfReq{
			TaskID: id, FreeBytes: 1, NeededEst: 1 << 60, Mount: mount,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("freeze-self %s expected 200, got %d: %s", id, w.Code, w.Body.String())
		}
	}
	for _, id := range ids {
		waitForProcessStopped(t, running[id].PID, true)
	}

	siblingID := h.addPendingTaskToJob(t, jobID)
	time.Sleep(200 * time.Millisecond)
	row, err := h.store.GetTask(h.ctx, siblingID)
	if err != nil {
		t.Fatalf("get sibling: %v", err)
	}
	if row == nil || row.Status != "pending" {
		t.Fatalf("sibling should stay pending while its job is frozen; row=%+v", row)
	}

	w = h.request(t, http.MethodPost, "/api/internal/freeze-self", api.FreezeSelfReq{
		TaskID: siblingID, FreeBytes: 1, NeededEst: 1, Mount: mount,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("pending task freeze expected 400, got %d: %s", w.Code, w.Body.String())
	}

	w = h.request(t, http.MethodPost, "/api/thaw?owner=-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("wrong-owner thaw expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeThawResponse(t, w)
	if len(resp.Thawed) != 0 || len(resp.Blocked) != 0 {
		t.Fatalf("wrong-owner thaw should be a no-op, got %+v", resp)
	}
	if !h.freeze.IsFrozen() {
		t.Fatal("wrong-owner thaw must not drain FreezeState")
	}

	w = h.request(t, http.MethodPost, fmt.Sprintf("/api/thaw?owner=%d", os.Getuid()), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("checked thaw expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp = decodeThawResponse(t, w)
	if len(resp.Thawed) != 0 {
		t.Fatalf("impossible threshold should thaw none, got %v", resp.Thawed)
	}
	if len(resp.Blocked) != taskCount {
		t.Fatalf("blocked count = %d, want %d: %+v", len(resp.Blocked), taskCount, resp.Blocked)
	}
	for _, id := range ids {
		br, ok := resp.Blocked[id]
		if !ok {
			t.Fatalf("%s missing from Blocked: %+v", id, resp.Blocked)
		}
		if br.Mount != mount {
			t.Fatalf("%s blocked mount = %q, want %q", id, br.Mount, mount)
		}
		if br.Threshold != 1<<60 {
			t.Fatalf("%s threshold = %d, want 1<<60", id, br.Threshold)
		}
	}
	if users := resp.Blocked[ids[0]].DiskUsers; len(users) > 0 {
		if len(users) < taskCount {
			t.Fatalf("disk users = %d, want at least %d: %+v", len(users), taskCount, users)
		}
		if users[0].TotalCkptBytes < users[len(users)-1].TotalCkptBytes {
			t.Fatalf("disk users not sorted by total checkpoint bytes desc: %+v", users)
		}
	}

	w = h.request(t, http.MethodPost, fmt.Sprintf("/api/thaw?owner=%d&force=true", os.Getuid()), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("force thaw expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp = decodeThawResponse(t, w)
	assertIDsEqualSet(t, resp.Thawed, ids)
	if len(resp.Blocked) != 0 {
		t.Fatalf("force thaw should not return blocked tasks: %+v", resp.Blocked)
	}
	if h.freeze.IsFrozen() {
		t.Fatal("force thaw should drain FreezeState")
	}
	for _, id := range ids {
		waitForProcessStopped(t, running[id].PID, false)
	}

	h.waitForDBStatus(t, siblingID, "running")
}
