package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/dashboard"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/scheduler"
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

	// Dashboard is the embedded HTTP dashboard. nil when dashboard.enabled
	// is false in config. Serves the Vue frontend + dashboard API on TCP.
	Dashboard       *dashboard.Server
	dashboardListen string // TCP address, e.g. "127.0.0.1:8077"

	// sshBackends holds SSHBackend references for cleanup on shutdown.
	sshBackends []*backend.SSHBackend

	// localTargetNames are the routing keys for local GPU targets. Used by
	// restore paths to avoid loading remote tasks into the local queue.
	// nil means legacy/test mode: restore all local-store rows.
	localTargetNames []string
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
		// Non-fatal: daemon can run without GPUs (e.g. Mac, CPU-only nodes).
		// Tasks requesting gpu_num > 0 will stay pending until GPUs appear.
		logger.Warn("GPU detection failed, running without GPU support", "error", err)
		gpus = nil
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
	// L2-C: FreezeState shared with the API server so `runq thaw` operates
	// on the same instance the scheduler manages. SocketPath is injected
	// into each task as RUNQ_SOCKET_PATH (reserved for stage 2+ SDK).
	freeze := scheduler.NewFreezeState()
	sched := scheduler.New(
		scheduler.DefaultConfig(),
		queue, pool, exec, st, logger, prioritizer,
		paths.SocketPath, freeze,
	)

	storageCfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	reg := project.NewRegistry(st.DB())

	// Determine the mode string for dashboard config response. With
	// targets[], the concept of a single mode is legacy; use "daemon" for
	// the local target's push-model semantics.
	mode := config.ConfigMode(storageCfg)

	// Build per-target backends. The local target always exists (daemon's
	// GPU pool); scheduler-type targets get SSHBackend wrappers.
	defaultTarget := storageCfg.ResolveDefaultTarget()

	resolvedTargets := storageCfg.ResolveTargets()
	makeLocalBackend := func(targetName string) *backend.LocalBackend {
		return backend.NewLocalBackend(backend.LocalBackendDeps{
			Store:      st,
			Reg:        reg,
			Queue:      queue,
			Scheduler:  sched,
			Exec:       exec,
			Pool:       pool,
			StorageCfg: storageCfg,
			TargetName: targetName,
		})
	}

	targets := make(map[string]backend.Backend)
	var sshBackends []*backend.SSHBackend
	var localTargetNames []string
	var localBe *backend.LocalBackend
	for _, tc := range resolvedTargets {
		if tc.Type() == config.TargetTypeHPC {
			sshBe, err := backend.NewSSHBackend(backend.SSHBackendConfig{
				Target:    tc,
				Store:     st,
				GlobalCfg: storageCfg,
			})
			if err != nil {
				return nil, fmt.Errorf("build SSH backend for target %q: %w", tc.Name, err)
			}
			targets[tc.Name] = sshBe
			sshBackends = append(sshBackends, sshBe)
			logger.Info("SSH backend registered", "target", tc.Name, "host", tc.SSH.Host)
		} else {
			// Local-type targets share daemon runtime components but keep separate
			// routing keys so DB target filters remain correct.
			be := makeLocalBackend(tc.Name)
			targets[tc.Name] = be
			localTargetNames = append(localTargetNames, tc.Name)
			if localBe == nil {
				localBe = be
			}
		}
	}
	if localBe == nil {
		// HPC-only configs still need a LocalBackend for store/registry helpers
		// used by API endpoints such as note resolution. It is not registered
		// as a routable target.
		localBe = makeLocalBackend("local")
	}

	multiBe, err := backend.NewMultiBackend(targets, st, defaultTarget)
	if err != nil {
		return nil, fmt.Errorf("build multi-backend: %w", err)
	}

	// API server — serves CLI over Unix socket. MultiBackend routes
	// target-aware operations (submit, kill, list, etc.).
	deps := api.Deps{
		Store:    st,
		Registry: reg,
		Queue:    queue,
		Pool:     pool,
		Logger:   logger,
		Local:    localBe,
		Multi:    multiBe,
		Freeze:   freeze,
	}
	server := api.NewServer(deps, paths.SocketPath, paths.PIDPath)

	d := &Daemon{
		Store:            st,
		Server:           server,
		Scheduler:        sched,
		Logger:           logger,
		Executor:         exec,
		Queue:            queue,
		Pool:             pool,
		sshBackends:      sshBackends,
		localTargetNames: localTargetNames,
	}

	// Embedded dashboard: start only when enabled in config.
	dashCfg := storageCfg.Dashboard
	if dashCfg == nil || dashCfg.Enabled {
		listen := "127.0.0.1:8077"
		if dashCfg != nil && dashCfg.Listen != "" {
			listen = dashCfg.Listen
		}
		d.Dashboard = dashboard.NewServer(multiBe, mode, storageCfg)
		d.dashboardListen = listen
		logger.Info("dashboard enabled", "listen", listen)
	}

	return d, nil
}

// Run starts the scheduler, API server (Unix socket), and embedded
// dashboard (TCP), then blocks until SIGINT/SIGTERM.
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

	// Start embedded dashboard on TCP (non-blocking).
	if d.Dashboard != nil {
		go d.serveDashboard()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		d.Shutdown(context.Background())
	}()

	// API server blocks on Unix socket.
	return d.Server.Start()
}

// serveDashboard starts the dashboard HTTP server on TCP. Errors are
// logged but do not bring down the daemon — the CLI/SDK path over Unix
// socket is the critical channel.
func (d *Daemon) serveDashboard() {
	ln, err := net.Listen("tcp", d.dashboardListen)
	if err != nil {
		d.Logger.Error("dashboard listen failed", "addr", d.dashboardListen, "error", err)
		return
	}
	d.Logger.Info("dashboard serving", "addr", d.dashboardListen)
	if d.Dashboard.StaticAssetsUnavailable() {
		d.Logger.Warn("dashboard UI not installed; API routes still available")
	}
	srv := &http.Server{Handler: d.Dashboard.Handler()}
	err = srv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		d.Logger.Error("dashboard serve error", "error", err)
	}
}

func (d *Daemon) restoreRuntimeState() error {
	// Phase 1: Reclaim previously-running tasks.
	// Reclaimer checks if their processes are still alive and updates DB accordingly.
	// Alive tasks get reattached (monitored via signal 0 polling).
	// Dead tasks get their DB status set to pending (retry) or failed.
	var aliveTasks []executor.AliveTask
	for _, targetName := range d.localRestoreTargets() {
		reclaimer := &executor.Reclaimer{
			Store:        d.Store,
			Exec:         d.Executor,
			Logger:       d.Logger,
			TargetFilter: targetName,
		}
		reclaimed, err := reclaimer.Reclaim(context.Background())
		if err != nil {
			d.Logger.Error("reclaim failed", "target", targetName, "error", err)
			continue
		}
		aliveTasks = append(aliveTasks, reclaimed...)
	}

	// Phase 2: Reserve GPUs, register alive tasks in Queue, and hand their
	// monitoring channels to the scheduler. The scheduler owns the lifecycle
	// from here — same cleanup path as normally dispatched tasks.
	for _, at := range aliveTasks {
		task := backend.TaskRowToSchedulerTask(&at.Row)
		if len(task.GPUs) == 0 {
			continue
		}
		if err := d.Pool.Reserve(task.GPUs, at.Row.ID); err != nil {
			d.Logger.Warn("GPU reserve failed for alive task",
				"task", at.Row.ID, "gpus", at.Row.GPUs, "error", err)
			continue
		}
		d.Queue.Restore(task)
		d.Scheduler.MonitorReattached(task, at.DoneCh)
		d.Logger.Info("alive task restored",
			"task", at.Row.ID, "pid", at.Row.PID, "gpus", task.GPUs)
	}

	// Restore paused job set from DB so pause semantics survive daemon restart.
	for _, targetName := range d.localRestoreTargets() {
		pausedJobs, err := d.Store.ListJobs(context.Background(), "", targetName)
		if err != nil {
			d.Logger.Warn("failed to load jobs for pause restore", "target", targetName, "error", err)
			continue
		}
		for _, j := range pausedJobs {
			if j.Status == "paused" {
				d.Scheduler.PauseJob(j.ID)
			}
		}
	}

	// Restore pending tasks from DB into the in-memory Queue.
	// This includes tasks that were originally pending AND dead tasks that
	// Reclaimer just set back to pending (resumable retry).
	var pendingCount int
	for _, targetName := range d.localRestoreTargets() {
		pendingTasks, err := d.Store.ListTasks(context.Background(), store.TaskFilter{Status: "pending", Target: targetName})
		if err != nil {
			return fmt.Errorf("load pending tasks for target %q from DB: %w", targetName, err)
		}
		for _, row := range pendingTasks {
			task := backend.TaskRowToSchedulerTask(&row)
			d.Queue.Restore(task)
		}
		pendingCount += len(pendingTasks)
	}
	if pendingCount > 0 {
		d.Logger.Info("restored pending tasks", "count", pendingCount)
	}
	return nil
}

func (d *Daemon) localRestoreTargets() []string {
	if d.localTargetNames == nil {
		return []string{""}
	}
	return d.localTargetNames
}

// Shutdown gracefully stops all daemon components.
func (d *Daemon) Shutdown(_ context.Context) {
	d.Logger.Info("shutdown signal received")
	if err := d.PidFile.Close(); err != nil {
		d.Logger.Warn("failed to close pid file!")
	}
	if d.Dashboard != nil {
		d.Dashboard.Close()
	}
	// Close SSH backends before the scheduler and store — outstanding SSH
	// operations should drain before the DB is closed.
	for _, sshBe := range d.sshBackends {
		if err := sshBe.Close(); err != nil {
			d.Logger.Warn("SSH backend close failed", "error", err)
		}
	}
	d.Scheduler.Shutdown()
	if err := d.Server.Shutdown(); err != nil {
		d.Logger.Error("server shutdown failed", "error", err)
	}
	if err := d.Store.Close(); err != nil {
		d.Logger.Error("db close failed", "error", err)
	}
}
