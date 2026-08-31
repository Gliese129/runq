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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/dashboard"
	"github.com/gliese129/runq-lab/internal/genfile"
	"github.com/gliese129/runq-lab/internal/gensync"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/utils"
)

// Daemon is the runq-lab client/dashboard process. Machine-local execution is
// owned by the independently installed runqd service.
type Daemon struct {
	Store   *store.Store
	Logger  *slog.Logger
	PidFile *os.File

	// Dashboard is the embedded HTTP dashboard. nil when dashboard.enabled
	// is false in config. Serves the Vue frontend + dashboard API on TCP.
	Dashboard       *dashboard.Server
	dashboardListen string // TCP address, e.g. "127.0.0.1:8077"

	// ── RQ-75: client-deployment lane registry (hot reload) ──
	//
	// lanes maps target name → live lane backend. It is MUTABLE: the config
	// reconciler adds/replaces/removes entries as config.yaml changes.
	// laneMu guards the map (not the lanes themselves — they have their own
	// lifecycles).
	laneMu sync.Mutex
	lanes  map[string]backend.Backend

	// multiBe is the routing backend the dashboard serves; the reconciler
	// mutates its target set in lockstep with d.lanes.
	multiBe *backend.MultiBackend

	// buildLane constructs one lane from its target config and captures the
	// data directory, store, and logger from assembly.
	buildLane func(tc config.TargetConfig, cfg *config.GlobalConfig) (backend.Backend, error)
	// buildHistoricalLane constructs a cold, artifact-only lane from a
	// persisted generation snapshot. It must not require the execution daemon
	// to be running because terminal artifact access is independent of runqd.
	buildHistoricalLane func(tc config.TargetConfig, cfg *config.GlobalConfig) (backend.Backend, error)

	// reconcileMu serializes ReconcileConfig passes (watcher tick, API
	// write notify, connect can all fire concurrently). Shutdown closes
	// ingress, sets shuttingDown, then takes this as a drain barrier.
	reconcileMu  sync.Mutex
	shutdownOnce sync.Once
	shuttingDown atomic.Bool
	// lastTargets / lastDefault are the reconciler's observed state — what
	// the running lanes were built from.
	lastTargets map[string]config.TargetConfig
	lastDefault string
	// bootDataPath detects restart-bound key changes (data_path is a
	// storage root — hot-swapping it would strand task dirs mid-flight).
	bootDataPath string
	// laneBuildErrs dedupes build-failure logging across retry passes
	// (target → last error string). Guarded by reconcileMu.
	laneBuildErrs map[string]string
	// laneCloseGrace delays closing a superseded lane so reads/streams that
	// grabbed the old pointer before the routing swap finish naturally
	// (0 = close synchronously; tests).
	laneCloseGrace time.Duration
	// retireZeroSeen: two-consecutive-zero close confirmation for retiring
	// lanes (round 5 #4). Serialized with the sweep (reconcileMu callers +
	// the single sweep ticker).
	retireZeroSeen map[string]bool
	// retiringLanes holds superseded lane generations still tracking their
	// in-flight tasks (RQ-75), keyed "name@generation". Guarded by laneMu;
	// registered in lockstep with multiBe's retiring registry.
	retiringLanes map[string]backend.Backend
	// historicalLanes are quiesced settled generations retained solely for
	// endpoint-correct log/metric reads. Guarded by laneMu.
	historicalLanes map[string]backend.Backend

	// cfgWatchCancel stops the config.yaml watcher goroutine on shutdown.
	cfgWatchCancel context.CancelFunc

	// forwards are the remote socket forwards (targets with remote_cli),
	// keyed by target name: each keeps the runq client socket on its login node
	// routed to this daemon's mux, so a remote `runq` CLI is just another
	// socket client. fwdMu guards the map — `runq connect` can start or
	// replace forwards at runtime via StartRemoteForward.
	fwdMu    sync.Mutex
	forwards map[string]*rfs.RemoteForward

	// contactRecorder, when set (client deployment), records a
	// daemon-observed reachability proof for one target — wired to
	// MultiBackend.RecordTargetContact and called from forward
	// OnEstablished hooks (RQ-74).
	contactRecorder func(name string)

	// pidPath is the client daemon's PID file. runqd owns a separate runtime
	// directory and lifecycle.
	pidPath string

	// Client deployment only: the unix socket the dashboard mux serves the
	// CLI on, and the http.Server wrapping it (for shutdown).
	socketPath string
	clientSrv  *http.Server
}

// NewDaemon creates the runq-lab client daemon: target routing, tracking, and
// the dashboard/CLI HTTP surface. It does not execute tasks in-process.
func NewDaemon() (*Daemon, error) {
	_, dataDir := utils.ResolveDataDir()
	paths := utils.PathsFromDataDir(dataDir)
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("initializing client daemon", "data_dir", dataDir)

	st, err := store.Open(paths.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	d, err := newClientDaemon(dataDir, paths, logger, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return d, nil
}

// Run starts the client lanes and dashboard/CLI HTTP surfaces, then blocks
// until SIGINT or SIGTERM.
func (d *Daemon) Run() error {
	pidPath := d.pidPath
	if pidPath == "" {
		_, dataDir := utils.ResolveDataDir()
		pidPath = utils.PathsFromDataDir(dataDir).PIDPath
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

	// Lanes restore themselves in Start(); recovery is a readiness barrier.
	// Copy the map so a failing Start can shut down without holding laneMu.
	d.laneMu.Lock()
	lanes := make(map[string]backend.Backend, len(d.lanes))
	for name, be := range d.lanes {
		lanes[name] = be
	}
	d.laneMu.Unlock()
	for name, be := range lanes {
		if err := startLane(be); err != nil {
			d.Shutdown(context.Background())
			return fmt.Errorf("recover target %s: %w", name, err)
		}
	}
	// RQ-75: retiring lane generations survive restarts — rebuild them
	// from their config snapshots so long-running old-generation tasks
	// keep being tracked on their original endpoints.
	if err := d.rebuildRetiringLanes(); err != nil {
		d.Shutdown(context.Background())
		return fmt.Errorf("recover retiring target generations: %w", err)
	}
	// RQ-75: watch config.yaml for semantic changes — the lane reconciler
	// converges running lanes onto the file without a restart. API writes
	// notify the same reconciler directly (SetConfigChanged).
	watchCtx, cancel := context.WithCancel(context.Background())
	d.cfgWatchCancel = cancel
	go gensync.WatchFile(watchCtx, config.ConfigPath(), 15*time.Second, d.Logger, func(*genfile.Doc) {
		if err := d.ReconcileConfig("config.yaml changed"); err != nil {
			d.Logger.Warn("config reconcile failed", "error", err)
		}
	})
	// Retirement sweep: close retiring lanes whose unfinished count hit
	// zero. Terminal transitions land via lane sensors at their own pace,
	// so a periodic sweep (plus one after every reconcile pass) is the
	// level-triggered way to notice.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-t.C:
				d.SweepRetiringLanes()
			}
		}
	}()
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

// StartRemoteForward (re)establishes the remote CLI forward for one target
// at runtime — the path behind POST /targets/{name}/connect. It first runs
// a reconcile pass: `runq connect` just wrote config.yaml, and a brand-new
// target's LANE is exactly that diff's add-branch (RQ-75 — this is what
// deleted errLaneRestartRequired; "restart to build a lane" is history).
// An existing forward is torn down and replaced (reconnect ceremony).
func (d *Daemon) StartRemoteForward(name string) error {
	if d.shuttingDown.Load() {
		return fmt.Errorf("daemon is shutting down")
	}
	if err := d.ReconcileConfig("connect"); err != nil {
		return err
	}
	if d.shuttingDown.Load() {
		return fmt.Errorf("daemon is shutting down")
	}
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
	}
	if d.buildLane != nil { // client deployment: the lane must exist by now
		d.laneMu.Lock()
		_, ok := d.lanes[name]
		d.laneMu.Unlock()
		if !ok {
			return fmt.Errorf("target %q has no lane — its build failed during reconcile; see daemon logs (Settings → runq logs)", name)
		}
	}
	return d.startForwardFor(*tc)
}

// startForwardFor builds and launches the remote CLI forward for one
// target, replacing any existing one. Shared by the reconciler (add/change
// branches) and StartRemoteForward.
func (d *Daemon) startForwardFor(tc config.TargetConfig) error {
	if d.shuttingDown.Load() {
		return fmt.Errorf("daemon is shutting down")
	}
	// RemoteCLIHandler, not Handler: the runtime path must wear the same
	// guard as the boot path — an unguarded forward is the RQ-45 escalation.
	fwd, err := newRemoteCLIForward(tc, d.Dashboard.RemoteCLIHandler(tc.Name), d.Logger, d.contactRecorderFor(tc.Name))
	if err != nil {
		return err
	}
	d.fwdMu.Lock()
	if d.shuttingDown.Load() {
		d.fwdMu.Unlock()
		_ = fwd.Close()
		return fmt.Errorf("daemon is shutting down")
	}
	if old, ok := d.forwards[tc.Name]; ok {
		_ = old.Close()
	}
	if d.forwards == nil {
		d.forwards = map[string]*rfs.RemoteForward{}
	}
	d.forwards[tc.Name] = fwd
	d.fwdMu.Unlock()
	go fwd.Run(context.Background())
	d.Logger.Info("remote CLI forward (re)started at runtime", "target", tc.Name)
	return nil
}

// ForwardStatuses snapshots every remote CLI forward's observable state,
// keyed by target name (RQ-74) — the /health "forwards" section.
func (d *Daemon) ForwardStatuses() map[string]rfs.ForwardStatus {
	d.fwdMu.Lock()
	defer d.fwdMu.Unlock()
	out := make(map[string]rfs.ForwardStatus, len(d.forwards))
	for name, fwd := range d.forwards {
		out[name] = fwd.Status()
	}
	return out
}

// contactRecorderFor binds the daemon's contact recorder to one target for
// use as a forward OnEstablished hook (RQ-74).
func (d *Daemon) contactRecorderFor(name string) func() {
	if d.contactRecorder == nil {
		return nil
	}
	return func() { d.contactRecorder(name) }
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
	d.shutdownOnce.Do(func() {
		// Reject lifecycle callbacks before closing their ingress. A callback
		// that already owns reconcileMu may finish; one queued behind it exits
		// after the barrier instead of touching a closed store.
		d.shuttingDown.Store(true)
		d.Logger.Info("shutdown signal received")
		if d.PidFile != nil {
			if err := d.PidFile.Close(); err != nil {
				d.Logger.Warn("failed to close pid file", "error", err)
			}
		}
		if d.Dashboard != nil {
			d.Dashboard.Close()
		}
		// Both HTTP listeners and the watcher are reconciliation ingress. Close
		// them before taking the lifecycle barrier.
		if d.clientSrv != nil {
			if err := d.clientSrv.Close(); err != nil {
				d.Logger.Error("client socket server close failed", "error", err)
			}
		}
		if d.cfgWatchCancel != nil {
			d.cfgWatchCancel()
		}

		// ReconcileConfig and SweepRetiringLanes both hold reconcileMu for the
		// complete lane transition. Waiting here joins an already-running
		// transition before taking the lane snapshot. The shuttingDown gate
		// makes it safe to release after the store has closed.
		d.reconcileMu.Lock()
		defer d.reconcileMu.Unlock()

		// A reconcile pass may create/replace a forward. Drain it first, then
		// close the final map under fwdMu; startForwardFor checks shuttingDown
		// while holding the same mutex, so no late forward can appear.
		d.fwdMu.Lock()
		for _, fwd := range d.forwards {
			_ = fwd.Close()
		}
		d.fwdMu.Unlock()

		// Close lanes before the store — outstanding SSH operations should
		// drain before the DB is closed.
		d.laneMu.Lock()
		for name, be := range d.lanes {
			if c, ok := be.(interface{ Close() error }); ok {
				if err := c.Close(); err != nil {
					d.Logger.Warn("lane close failed", "target", name, "error", err)
				}
			}
		}
		for key, be := range d.retiringLanes {
			if c, ok := be.(interface{ Close() error }); ok {
				if err := c.Close(); err != nil {
					d.Logger.Warn("retiring lane close failed", "lane", key, "error", err)
				}
			}
		}
		for key, be := range d.historicalLanes {
			if c, ok := be.(interface{ Close() error }); ok {
				if err := c.Close(); err != nil {
					d.Logger.Warn("historical lane close failed", "lane", key, "error", err)
				}
			}
		}
		d.laneMu.Unlock()
		if err := d.Store.Close(); err != nil {
			d.Logger.Error("db close failed", "error", err)
		}
	})
}
