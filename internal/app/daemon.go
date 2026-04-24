package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
	sched := scheduler.New(scheduler.DefaultConfig(), queue, pool, exec, st, logger)

	// Build service layer.
	reg := project.NewRegistry(st.DB())
	jobSvc := &service.JobService{
		Store: st, Queue: queue, Scheduler: sched, Exec: exec, Registry: reg,
	}
	taskSvc := &service.TaskService{
		Store: st, Queue: queue, Exec: exec,
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
	}, nil
}

// Run starts the scheduler and API server, blocks until SIGINT/SIGTERM.
func (d *Daemon) Run() error {
	d.Scheduler.Start()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		d.Shutdown(context.Background())
	}()

	return d.Server.Start()
}

// Shutdown gracefully stops all daemon components.
func (d *Daemon) Shutdown(_ context.Context) {
	d.Logger.Info("shutdown signal received")
	d.Scheduler.Shutdown()
	if err := d.Server.Shutdown(); err != nil {
		d.Logger.Error("server shutdown failed", "error", err)
	}
	if err := d.Store.Close(); err != nil {
		d.Logger.Error("db close failed", "error", err)
	}
}
