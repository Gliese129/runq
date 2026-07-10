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
	"sync"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/dashboard"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/rfs"
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

	// forwards are the remote socket forwards (targets with remote_cli),
	// keyed by target name: each keeps ~/.runq/runq.sock on its login node
	// routed to this daemon's mux, so a remote `runq` CLI is just another
	// socket client. fwdMu guards the map — `runq connect` can start or
	// replace forwards at runtime via StartRemoteForward.
	fwdMu    sync.Mutex
	forwards map[string]*rfs.RemoteForward

	// laneNames records which targets got a lane at assembly time. Lanes
	// are NOT hot-reloaded (restart-bound by design); StartRemoteForward
	// uses this to tell "start the forward now" apart from "the whole
	// target is new — a restart is genuinely needed".
	laneNames map[string]bool

	// pidPath is this deployment's PID file (client: daemon.pid,
	// runqd: runqd.pid) — the two daemons coexist on one machine.
	pidPath string

	// Client deployment only: the unix socket the dashboard mux serves the
	// CLI on, and the http.Server wrapping it (for shutdown).
	socketPath string
	clientSrv  *http.Server

	// localTargetNames are the routing keys for local GPU targets. Used by
	// restore paths to avoid loading remote tasks into the local queue.
	// nil means legacy/test mode: restore all local-store rows.
	localTargetNames []string
}

// DaemonOptions selects which surfaces a daemon exposes.
type DaemonOptions struct {
	// Headless is the server deployment (`runqd` on a lab GPU box):
	// scheduler + executor + store + socket API only. It forces the
	// dashboard off and REFUSES ssh targets — a server manages exactly its
	// own hardware; routing to other machines is the client's job. Keeping
	// this fail-closed prevents a second control plane growing on a shared
	// machine by accident.
	Headless bool
}

// NewDaemon creates and wires all daemon components (client deployment:
// all configured targets + dashboard). Does NOT start them — call Run().
func NewDaemon() (*Daemon, error) {
	return NewDaemonWith(DaemonOptions{})
}

// NewDaemonWith creates a daemon with explicit surface options.
func NewDaemonWith(opts DaemonOptions) (*Daemon, error) {
	_, dataDir := utils.ResolveDataDir()
	// Two deployments, two file sets under one data root: the client keeps
	// every legacy name (runq.db/runq.sock/daemon.pid — history and socket
	// paths survive the split); runqd gets its own store, socket and pid.
	paths := utils.PathsFromDataDir(dataDir)
	if opts.Headless {
		paths = utils.RunqdPathsFromDataDir(dataDir)
	}

	// Ensure data and log directories exist.
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("initializing daemon", "data_dir", dataDir)

	// RQ-45 startup gate (same rationale as the client daemon).
	if warn, perr := config.CheckConfigPermissions(); perr != nil {
		return nil, perr
	} else if warn != "" {
		logger.Warn(warn)
	}

	// Open DB (auto-migrates schema).
	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Client deployment: routing + tracking + dashboard only — assembled in
	// client_daemon.go. Everything below this line is the runqd (headless
	// execution) assembly: local lane + gin API.
	if !opts.Headless {
		return newClientDaemon(dataDir, paths, logger, st)
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
		queue, pool, scheduler.NewLocalLauncher(exec), st, logger, prioritizer,
		paths.SocketPath, freeze,
	)

	storageCfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	reg := project.NewRegistry(st.DB())

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
			if opts.Headless {
				return nil, fmt.Errorf(
					"target %q: ssh targets are not supported in server mode — configure remote targets on the client, the server only manages its own hardware", tc.Name)
			}
			sshBe, err := backend.NewSSHBackend(backend.SSHBackendConfig{
				Target:    tc,
				Store:     st,
				GlobalCfg: storageCfg,
				Logger:    logger,
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
		pidPath:          paths.PIDPath,
	}

	// No dashboard here: this is the runqd assembly, and the server stays
	// headless — a server-side web UI would need its own auth story (who
	// sees whose jobs), which violates the "SSH is the auth boundary"
	// principle. The dashboard belongs to the client (client_daemon.go).
	return d, nil
}

// Run starts the scheduler, API server (Unix socket), and embedded
// dashboard (TCP), then blocks until SIGINT/SIGTERM.
func (d *Daemon) Run() error {
	pidPath := d.pidPath
	if pidPath == "" {
		pidPath = api.DefaultPIDPath() // legacy/test construction path
	}
	pidFile, err := utils.LockFile(pidPath)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(pidFile, "%d,%s", os.Getpid(), time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	d.PidFile = pidFile

	// runqd (execution daemon): restore local lane, start the scheduler,
	// serve the gin API on the socket.
	if d.Server != nil {
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

		// API server blocks on Unix socket.
		return d.Server.Start()
	}

	// Client daemon: lanes restore themselves in Start(); the dashboard mux
	// serves both listeners (unix socket for the CLI, TCP for the browser).
	for _, sshBe := range d.sshBackends {
		sshBe.Start(context.Background())
	}
	// Remote CLI forwards (targets with remote_cli): each supervises its
	// own reconnect loop; failures never block the daemon.
	d.fwdMu.Lock()
	for _, fwd := range d.forwards {
		go fwd.Run(context.Background())
	}
	d.fwdMu.Unlock()
	if d.dashboardListen != "" {
		go d.serveDashboard()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		d.Shutdown(context.Background())
	}()

	return d.serveClientSocket()
}

// serveClientSocket serves the dashboard mux on the client's unix socket —
// the CLI's channel. Blocks until Shutdown.
func (d *Daemon) serveClientSocket() error {
	// Stale socket from an unclean exit: the PID lock above guarantees we
	// are the only client daemon, so removing it is safe.
	_ = os.Remove(d.socketPath)
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen on client socket %s: %w", d.socketPath, err)
	}
	if err := os.Chmod(d.socketPath, 0o600); err != nil {
		d.Logger.Warn("chmod client socket failed", "error", err)
	}
	d.Logger.Info("client daemon serving", "socket", d.socketPath)
	d.clientSrv = &http.Server{Handler: d.Dashboard.Handler()}
	err = d.clientSrv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
// errLaneRestartRequired distinguishes "target added after daemon start"
// from forward-level problems: lanes are restart-bound by design, so this
// is the one case where `runq connect` must still say "restart". Wraps the
// dashboard sentinel so the /connect handler can map it to 409.
var errLaneRestartRequired = fmt.Errorf("%w: target has no lane in the running daemon — `runq daemon restart` to build one", dashboard.ErrForwardRestartRequired)

// StartRemoteForward (re)establishes the remote CLI forward for one target
// at runtime — the path behind POST /targets/{name}/connect, so `runq
// connect` takes effect without a daemon restart. It re-reads config.yaml
// (connect just wrote it); an existing forward for the target is torn down
// and replaced (idempotent — reconnect ceremony semantics).
func (d *Daemon) StartRemoteForward(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var tc *config.TargetConfig
	targets := cfg.ResolveTargets()
	for i := range targets {
		if targets[i].Name == name {
			tc = &targets[i]
			break
		}
	}
	switch {
	case tc == nil:
		return fmt.Errorf("target %q not found", name)
	case tc.SSH == nil:
		return fmt.Errorf("target %q has no ssh section", name)
	case !tc.RemoteCLI:
		return fmt.Errorf("target %q does not have remote_cli enabled", name)
	case !d.laneNames[name]:
		return errLaneRestartRequired
	}

	// RemoteCLIHandler, not Handler: the runtime path must wear the same
	// guard as the boot path — an unguarded forward is the RQ-45 escalation.
	fwd, err := newRemoteCLIForward(*tc, d.Dashboard.RemoteCLIHandler(name), d.Logger)
	if err != nil {
		return err
	}
	d.fwdMu.Lock()
	if old, ok := d.forwards[name]; ok {
		_ = old.Close()
	}
	if d.forwards == nil {
		d.forwards = map[string]*rfs.RemoteForward{}
	}
	d.forwards[name] = fwd
	d.fwdMu.Unlock()
	go fwd.Run(context.Background())
	d.Logger.Info("remote CLI forward (re)started at runtime", "target", name)
	return nil
}

// StopRemoteForward tears down one target's remote CLI forward at runtime
// — the path behind POST /targets/{name}/disconnect. Idempotent: no
// forward is a success, not an error.
func (d *Daemon) StopRemoteForward(name string) error {
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	if fwd, ok := d.forwards[name]; ok {
		_ = fwd.Close()
		delete(d.forwards, name)
		d.Logger.Info("remote CLI forward stopped", "target", name)
	}
	return nil
}

func (d *Daemon) Shutdown(_ context.Context) {
	d.Logger.Info("shutdown signal received")
	if err := d.PidFile.Close(); err != nil {
		d.Logger.Warn("failed to close pid file!")
	}
	if d.Dashboard != nil {
		d.Dashboard.Close()
	}
	// Remote CLI forwards go first: they feed requests INTO the mux, so
	// stop accepting remote traffic before draining the lanes below it.
	d.fwdMu.Lock()
	for _, fwd := range d.forwards {
		_ = fwd.Close()
	}
	d.fwdMu.Unlock()
	// Close lanes before the scheduler and store — outstanding SSH
	// operations should drain before the DB is closed.
	for _, sshBe := range d.sshBackends {
		if err := sshBe.Close(); err != nil {
			d.Logger.Warn("lane close failed", "error", err)
		}
	}
	// Deployment-specific surfaces: runqd has Scheduler+gin Server; the
	// client has the socket http.Server. Guard both — one Daemon type, two
	// assemblies.
	if d.Scheduler != nil {
		d.Scheduler.Shutdown()
	}
	if d.Server != nil {
		if err := d.Server.Shutdown(); err != nil {
			d.Logger.Error("server shutdown failed", "error", err)
		}
	}
	if d.clientSrv != nil {
		if err := d.clientSrv.Close(); err != nil {
			d.Logger.Error("client socket server close failed", "error", err)
		}
	}
	if err := d.Store.Close(); err != nil {
		d.Logger.Error("db close failed", "error", err)
	}
}
