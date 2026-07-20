package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/dashboard"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
)

// newClientDaemon assembles the CLIENT deployment (4d): routing + tracking +
// dashboard, and nothing else. No executor, no GPU pool, no local scheduler
// lane, no gin API — execution lives in runqd (local or remote), and every
// target, including this machine's own GPUs, is a remote-machinery lane:
//
//	remote HPC:     slurm/pbs preset over rfs.SSHFS
//	remote runqd:   runq preset over rfs.SSHFS
//	local runqd:    runq preset over rfs.LocalFS (+ ensure-running)
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

	// buildLane is THE lane constructor — boot and the RQ-75 reconciler
	// build lanes through the same function, so a runtime rebuild can never
	// diverge from what a restart would have produced.
	buildLane := func(tc config.TargetConfig, cfg *config.GlobalConfig) (backend.Backend, error) {
		if tc.Type() == config.TargetTypeRemote {
			be, berr := backend.NewSSHBackend(backend.SSHBackendConfig{
				Target: tc, Store: st, GlobalCfg: cfg, Logger: logger,
			})
			if berr == nil {
				logger.Info("remote lane registered", "target", tc.Name, "host", tc.SSH.Host)
			}
			return be, berr
		}
		// Local-GPUs target: synthesize a localhost-runqd lane. Same
		// machinery, LocalFS transport; plumbing commands default to
		// runqd.sock, so the preset templates are verbatim portable.
		synth, serr := synthLocalRunqdTarget(tc, dataDir)
		if serr != nil {
			return nil, fmt.Errorf("target %q: %w", tc.Name, serr)
		}
		ensureRunqd(logger, dataDir)
		be, berr := backend.NewSSHBackend(backend.SSHBackendConfig{
			Target: synth, Store: st, GlobalCfg: cfg, Logger: logger,
			FS: rfs.NewLocalFS(),
		})
		if berr == nil {
			logger.Info("localhost runqd lane registered", "target", tc.Name)
		}
		return be, berr
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
		Store:        st,
		Logger:       logger,
		lanes:        laneMap,
		multiBe:      multiBe,
		buildLane:    buildLane,
		lastTargets:  targetsByName(storageCfg),
		lastDefault:  defaultTarget,
		bootDataPath: storageCfg.DataPath,
		pidPath:      paths.PIDPath,
		socketPath:   paths.SocketPath,
	}

	// The dashboard mux is the client's ONLY server surface: always built
	// (the CLI socket needs it); TCP listening is what dashboard.enabled
	// gates.
	d.Dashboard = dashboard.NewServer(multiBe, storageCfg)

	// Remote CLI forwards (remote_cli targets): a dedicated SSH connection
	// per target keeps ~/.runq/runq.sock on the login node routed straight
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

	// PATH independence: the daemon may run under launchd/systemd where
	// `runq` is not on PATH. Rewrite the preset's leading `runq ` to this
	// executable's absolute path (shell-quoted — paths can contain spaces).
	if self, serr := os.Executable(); serr == nil {
		selfQ := utils.ShellQuote(self) + " "
		for _, f := range []*string{
			&out.SubmitTemplate, &out.StatusTemplate,
			&out.StatusListTemplate, &out.KillTemplate, &out.GPUTemplate,
		} {
			if strings.HasPrefix(*f, "runq ") {
				*f = selfQ + strings.TrimPrefix(*f, "runq ")
			}
		}
		// Same PATH-independence for compute-node work inside run.sh
		// (pyramid build): a local task's "compute node" is this machine.
		out.RunqBin = self
	}
	return out, nil
}

// ensureRunqd starts this machine's execution daemon when its socket is not
// answering. Best-effort and fire-and-forget: runqd owns its own lifecycle
// (crash isolation is the point of the split), so the client never
// supervises or stops it — it only makes sure one exists.
func ensureRunqd(logger *slog.Logger, dataDir string) {
	socket := utils.RunqdPathsFromDataDir(dataDir).SocketPath
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		_ = conn.Close()
		return
	}

	bin := FindRunqd()
	if bin == "" {
		logger.Warn("runqd binary not found (looked next to runq and on PATH) — local target will fail until runqd is started manually")
		return
	}

	logDir := utils.PathsFromDataDir(dataDir).LogDir
	_ = os.MkdirAll(logDir, 0o755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "runqd.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logger.Warn("open runqd log failed", "error", err)
		return
	}

	cmd := exec.Command(bin)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logger.Warn("start runqd failed", "error", err)
		_ = logFile.Close()
		return
	}
	// Detach: the child outlives us by design.
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()

	// Wait for the socket to answer (≤5s). Without this, a submit fired
	// right after daemon start races runqd's boot — and since a transport
	// failure sends the task back to pending, the race would only cost a
	// tick, but a clean readiness gate keeps first-run logs quiet and
	// first-submit latency deterministic.
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			logger.Info("runqd started", "pid", cmd.Process.Pid, "bin", bin)
			return
		}
	}
	logger.Warn("runqd started but socket not ready within 5s — local submissions will retry per tick",
		"pid", cmd.Process.Pid, "socket", socket)
}

// FindRunqd locates the runqd binary: sibling of the current executable
// first (the normal install layout), then PATH.
func FindRunqd() string {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "runqd")
		if st, err := os.Stat(sibling); err == nil && !st.IsDir() {
			return sibling
		}
	}
	if p, err := exec.LookPath("runqd"); err == nil {
		return p
	}
	return ""
}
