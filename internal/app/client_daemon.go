package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/dashboard"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/utils"
)

// newClientDaemon assembles the CLIENT deployment (4d): routing + tracking +
// dashboard, and nothing else. No in-process runner, GPU pool, local scheduler
// lane, or machine API — execution lives in runqd (local or remote), and every
// target, including this machine's own GPUs, is a remote-machinery lane:
//
//	remote HPC:     slurm/pbs preset over rfs.SSHFS
//	remote runqd:   runq preset over rfs.SSHFS
//	local runqd:    runq preset over rfs.LocalFS (pre-started independently)
//
// The client serves ONE route table (the dashboard mux) on two listeners:
// the unix socket for the CLI and TCP for the browser.
func newClientDaemon(dataDir string, paths utils.DataDirPaths, logger *slog.Logger, st *store.Store) (*Daemon, error) {
	// RQ-45 startup gate: a tamperable config file feeds ssh hosts and
	// submit templates to a daemon that EXECUTES them.
	if warn, perr := config.CheckConfigPermissions(); perr != nil {
		return nil, perr
	} else if warn != "" {
		logger.Warn(warn)
	}

	storageCfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	defaultTarget := storageCfg.ResolveDefaultTarget()

	// Active and historical lanes share one constructor. Historical lanes are
	// cold filesystem readers, so they must not require a live local runqd
	// endpoint merely to expose terminal artifacts.
	buildLaneFor := func(tc config.TargetConfig, cfg *config.GlobalConfig, requireLocalEndpoint bool) (backend.Backend, error) {
		if tc.Type() == config.TargetTypeRemote {
			return backend.NewSSHBackend(backend.SSHBackendConfig{
				Target: tc, Store: st, GlobalCfg: cfg, Logger: logger,
			})
		}
		// Local-GPUs target: synthesize a localhost-runqd lane. Same
		// machinery, LocalFS transport; plumbing commands default to
		// runqd.sock, so the preset templates are verbatim portable.
		synth, serr := synthLocalRunqdTarget(tc, dataDir)
		if serr != nil {
			return nil, fmt.Errorf("target %q: %w", tc.Name, serr)
		}
		if requireLocalEndpoint {
			if cerr := connectRunqdEndpoint(utils.RunqdSocketPath()); cerr != nil {
				return nil, fmt.Errorf("target %q: %w", tc.Name, cerr)
			}
		}
		return backend.NewSSHBackend(backend.SSHBackendConfig{
			Target: synth, Store: st, GlobalCfg: cfg, Logger: logger,
			FS: rfs.NewLocalFS(),
		})
	}
	// buildLane is THE active lane constructor — boot and hot reconcile use
	// the same readiness contract.
	buildLane := func(tc config.TargetConfig, cfg *config.GlobalConfig) (backend.Backend, error) {
		return buildLaneFor(tc, cfg, true)
	}
	buildHistoricalLane := func(tc config.TargetConfig, cfg *config.GlobalConfig) (backend.Backend, error) {
		return buildLaneFor(tc, cfg, false)
	}

	targets := make(map[string]backend.Backend)
	laneMap := make(map[string]backend.Backend)
	for _, tc := range storageCfg.ResolveTargets() {
		be, berr := buildLane(tc, storageCfg)
		if berr != nil {
			return nil, fmt.Errorf("build lane for target %q: %w", tc.Name, berr)
		}
		targets[tc.Name] = be
		laneMap[tc.Name] = be
	}

	multiBe, err := backend.NewMultiBackend(targets, st, defaultTarget)
	if err != nil {
		return nil, fmt.Errorf("build multi-backend: %w", err)
	}

	d := &Daemon{
		Store:               st,
		Logger:              logger,
		lanes:               laneMap,
		multiBe:             multiBe,
		buildLane:           buildLane,
		buildHistoricalLane: buildHistoricalLane,
		lastTargets:         targetsByName(storageCfg),
		lastDefault:         defaultTarget,
		bootDataPath:        storageCfg.DataPath,
		pidPath:             paths.PIDPath,
		socketPath:          paths.SocketPath,
		// Superseded lanes with nothing left to track keep serving
		// already-started reads/streams for 60s before closing.
		laneCloseGrace:  60 * time.Second,
		historicalLanes: map[string]backend.Backend{},
	}

	// The dashboard mux is the client's ONLY server surface: always built
	// (the CLI socket needs it); TCP listening is what dashboard.enabled
	// gates.
	d.Dashboard = dashboard.NewServer(multiBe, storageCfg)

	// Remote CLI forwards (remote_cli targets): a dedicated SSH connection
	// per target keeps the runq client socket on the login node routed straight
	// into the mux above — the remote `runq` is just another socket client.
	// Serve reuses the full middleware chain (version gate included).
	d.forwards = map[string]*rfs.RemoteForward{}
	// Forward establishment doubles as a reachability proof (RQ-74):
	// OnEstablished records lane contact so doctor's "no contact yet"
	// clears the moment the forward is up.
	d.contactRecorder = multiBe.RecordTargetContact
	for _, tc := range storageCfg.ResolveTargets() {
		if !tc.RemoteCLI || tc.SSH == nil {
			continue
		}
		fwd, ferr := newRemoteCLIForward(tc, d.Dashboard.RemoteCLIHandler(tc.Name), logger, d.contactRecorderFor(tc.Name))
		if ferr != nil {
			// Non-fatal by design: the lane itself still works; the user
			// just doesn't get the remote CLI until the config is fixed.
			logger.Warn("remote CLI forward not started", "target", tc.Name, "error", ferr)
			continue
		}
		d.forwards[tc.Name] = fwd
		logger.Info("remote CLI forward registered", "target", tc.Name, "host", tc.SSH.Host)
	}
	// `runq connect` starts/replaces forwards at runtime through this hook
	// (POST /targets/{name}/connect) — no daemon restart for the forward.
	d.Dashboard.SetForwardStarter(d.StartRemoteForward)
	d.Dashboard.SetForwardStopper(d.StopRemoteForward)
	d.Dashboard.SetForwardStatus(d.ForwardStatuses)
	// RQ-75: API writes to config.yaml notify the reconciler directly —
	// the file watcher would catch them anyway, one tick later.
	d.Dashboard.SetConfigChanged(func() {
		go func() {
			if err := d.ReconcileConfig("api write"); err != nil {
				logger.Warn("config reconcile failed", "error", err)
			}
		}()
	})
	dashCfg := storageCfg.Dashboard
	if dashCfg == nil || dashCfg.Enabled {
		listen := "127.0.0.1:8077"
		if dashCfg != nil && dashCfg.Listen != "" {
			listen = dashCfg.Listen
		}
		d.dashboardListen = listen
		logger.Info("dashboard enabled", "listen", listen)
		if h, _, err := net.SplitHostPort(listen); err == nil && h != "127.0.0.1" && h != "localhost" && h != "::1" {
			logger.Warn("dashboard bound to a non-loopback address — the API has NO authentication; prefer 127.0.0.1 + ssh -L tunneling", "listen", listen)
		}
	}
	return d, nil
}

// newRemoteCLIForward builds the reverse socket forward for one remote_cli
// target. SSH construction mirrors backend.NewSSHBackend (same host:port and
// auth resolution) but deliberately NOT the same connection: the forward is
// a persistent service, exempt from the lane's idle-disconnect etiquette
// (see rfs.RemoteForward).
func newRemoteCLIForward(tc config.TargetConfig, handler http.Handler, logger *slog.Logger, onEstablished func()) (*rfs.RemoteForward, error) {
	// Same alias resolution as the lane: `host:` may be an ~/.ssh/config
	// alias; explicit target fields win over ssh_config values.
	sshHost, sshPort, sshUser, sshKey := rfs.ResolveSSHConfigDefaults(
		tc.SSH.Host, tc.SSH.Port, tc.SSH.User, tc.SSH.Key)
	host := sshHost
	if sshPort > 0 {
		host = fmt.Sprintf("%s:%d", sshHost, sshPort)
	}
	auth, err := rfs.ResolveAuthMethods(sshKey)
	if err != nil {
		return nil, fmt.Errorf("ssh auth: %w", err)
	}
	return rfs.NewRemoteForward(rfs.RemoteForwardConfig{
		SSH: rfs.SSHConfig{
			Host:        host,
			User:        sshUser,
			AuthMethods: auth,
			// RQ-74: same accept-new passthrough as the lane connection.
			HostKeyPolicy: rfs.ResolveHostKeyPolicy(tc.SSH.Host),
		},
		Serve: func(ln net.Listener) error {
			err := (&http.Server{Handler: handler}).Serve(ln)
			if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
				return nil // orderly teardown, not a failure
			}
			return err
		},
		OnEstablished: onEstablished,
		Logger:        logger.With("target", tc.Name),
	}), nil
}

// synthLocalRunqdTarget expands the runq preset for a local-GPUs target: the
// templates come straight from the preset table (identical to a remote runqd
// target); only identity and marker root are local specifics. Workspace is
// deliberately left empty so task dirs keep the per-project layout
// (ResolveRoot), matching pre-split local behavior.
func synthLocalRunqdTarget(tc config.TargetConfig, dataDir string) (config.TargetConfig, error) {
	pt, err := config.HPCPreset("runq")
	if err != nil {
		return tc, fmt.Errorf("expand runq preset: %w", err)
	}
	out := *pt
	out.Name = tc.Name
	out.GPUs = tc.GPUs // informational; allocation is runqd's job
	out.MaxInflight = tc.MaxInflight
	out.DoneDir = filepath.Join(dataDir, "runq-done")
	// Metrics indexing remains runq-lab-owned. A local task's compute node is
	// this machine, so retain the client's absolute path for that optional
	// post-processing hook without using it as runqd's control adapter.
	if self, serr := os.Executable(); serr == nil {
		out.RunqBin = self
	}
	return out, nil
}

// connectRunqdEndpoint verifies the configured execution endpoint without
// locating, launching, supervising, or otherwise owning runqd. The execution
// daemon is an independent service and must be ready before the runq client
// daemon starts a local machine lane.
func connectRunqdEndpoint(socket string) error {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return fmt.Errorf(
			"runqd is not reachable at %q: %w; start runqd independently (for example, `runqd serve`) or set RUNQD_SOCKET to its Unix socket",
			socket, err,
		)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close runqd connection at %q: %w", socket, err)
	}
	return nil
}
