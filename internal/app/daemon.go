package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// Daemon holds all runtime components of the runq daemon.
type Daemon struct {
	Store     *store.Store
	Server    *api.Server
	Scheduler *scheduler.Scheduler
	Logger    *slog.Logger
	PidFile   *os.File
	Executor  *executor.Executor
	Queue     *scheduler.Queue
	Pool      resource.Allocator
}

// NewDaemon creates and wires all daemon components.
// Does NOT start them — call Run() for that.
func NewDaemon() (*Daemon, error) {
	_, dataDir := utils.ResolveDataDir()
	paths := utils.PathsFromDataDir(dataDir)

	// Ensure data and log directories exist.
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("initializing daemon", "data_dir", dataDir)

	// Open DB (auto-migrates schema).
	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	gpus, err := resource.Detect()
	if err != nil {
		return nil, fmt.Errorf("detect GPUs: %w", err)
	}

	pool := resource.NewGPUPool(gpus)
	queue := scheduler.NewQueue()
	exec := executor.New()

	// Default to fair-share scheduling with 24h sliding window.
	// TODO: make configurable via runq config (fifo / fair).
	prioritizer := &scheduler.FairSharePrioritizer{
		Store:  st,
		Window: 24 * time.Hour,
	}
	sched := scheduler.New(scheduler.DefaultConfig(), queue, pool, exec, st, logger, prioritizer)

	// Build service layer.
	reg := project.NewRegistry(st.DB())
	jobSvc := &service.JobService{
		Store: st, Queue: queue, Scheduler: sched, Exec: exec, Registry: reg, Pool: pool,
	}
	taskSvc := &service.TaskService{
		Store: st, Queue: queue, Exec: exec, Scheduler: sched,
	}

	deps := api.Deps{
		Store:       st,
		Registry:    reg,
		Scheduler:   sched,
		Queue:       queue,
		Pool:        pool,
		Executor:    exec,
		Logger:      logger,
		JobService:  jobSvc,
		TaskService: taskSvc,
	}

	server := api.NewServer(deps, paths.SocketPath, paths.PIDPath)

	return &Daemon{
		Store:     st,
		Server:    server,
		Scheduler: sched,
		Logger:    logger,
		Executor:  exec,
		Queue:     queue,
		Pool:      pool,
	}, nil
}

// Run starts the scheduler and API server, blocks until SIGINT/SIGTERM.
func (d *Daemon) Run() error {
	pidFile, err := utils.LockFile(api.DefaultPIDPath())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(pidFile, "%d,%s", os.Getpid(), time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	d.PidFile = pidFile

	if err := d.restoreRuntimeState(); err != nil {
		return err
	}

	d.Scheduler.Start()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		d.Shutdown(context.Background())
	}()

	return d.Server.Start()
}

func (d *Daemon) restoreRuntimeState() error {
	// Phase 1: Reclaim previously-running tasks.
	// Reclaimer checks if their processes are still alive and updates DB accordingly.
	// Alive tasks get reattached (monitored via signal 0 polling).
	// Dead tasks get their DB status set to pending (retry) or failed.
	reclaimer := &executor.Reclaimer{
		Store:  d.Store,
		Exec:   d.Executor,
		Logger: d.Logger,
	}
	aliveTasks, err := reclaimer.Reclaim()
	if err != nil {
		d.Logger.Error("reclaim failed", "error", err)
	}

	// Phase 2: Reserve GPUs for alive tasks so the pool reflects actual usage.
	for _, t := range aliveTasks {
		gpuIndices := parseGPUIndices(t.GPUs)
		if len(gpuIndices) == 0 {
			continue
		}
		if err := d.Pool.Reserve(gpuIndices, t.ID); err != nil {
			d.Logger.Warn("GPU reserve failed for alive task",
				"task", t.ID, "gpus", t.GPUs, "error", err)
		} else {
			d.Logger.Info("GPUs reserved for alive task", "task", t.ID, "gpus", gpuIndices)
		}
	}

	// Restore paused job set from DB so pause semantics survive daemon restart.
	pausedJobs, err := d.Store.ListJobs(context.Background(), "")
	if err != nil {
		d.Logger.Warn("failed to load jobs for pause restore", "error", err)
	} else {
		for _, j := range pausedJobs {
			if j.Status == "paused" {
				d.Scheduler.PauseJob(j.ID)
			}
		}
	}

	// Restore pending tasks from DB into the in-memory Queue.
	// This includes tasks that were originally pending AND dead tasks that
	// Reclaimer just set back to pending (resumable retry).
	pendingTasks, err := d.Store.ListTasks(context.Background(), store.TaskFilter{Status: "pending"})
	if err != nil {
		return fmt.Errorf("load pending tasks from DB: %w", err)
	}
	for _, row := range pendingTasks {
		task := service.TaskRowToSchedulerTask(&row)
		d.Queue.Push(task)
	}
	if len(pendingTasks) > 0 {
		d.Logger.Info("restored pending tasks", "count", len(pendingTasks))
	}
	return nil
}

// parseGPUIndices converts a comma-separated GPU string (e.g. "0,1,3") to []int.
func parseGPUIndices(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	indices := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		indices = append(indices, v)
	}
	return indices
}

// Shutdown gracefully stops all daemon components.
func (d *Daemon) Shutdown(_ context.Context) {
	d.Logger.Info("shutdown signal received")
	if err := d.PidFile.Close(); err != nil {
		d.Logger.Warn("failed to close pid file!")
	}
	d.Scheduler.Shutdown()
	if err := d.Server.Shutdown(); err != nil {
		d.Logger.Error("server shutdown failed", "error", err)
	}
	if err := d.Store.Close(); err != nil {
		d.Logger.Error("db close failed", "error", err)
	}
}
