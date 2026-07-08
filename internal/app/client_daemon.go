package app

import (
	"fmt"
	"log/slog"
	"net"
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
	storageCfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	defaultTarget := storageCfg.ResolveDefaultTarget()

	targets := make(map[string]backend.Backend)
	var lanes []*backend.SSHBackend
	for _, tc := range storageCfg.ResolveTargets() {
		var be *backend.SSHBackend
		var berr error
		if tc.Type() == config.TargetTypeHPC {
			be, berr = backend.NewSSHBackend(backend.SSHBackendConfig{
				Target: tc, Store: st, GlobalCfg: storageCfg, Logger: logger,
			})
			if berr == nil {
				logger.Info("remote lane registered", "target", tc.Name, "host", tc.SSH.Host)
			}
		} else {
			// Local-GPUs target: synthesize a localhost-runqd lane. Same
			// machinery, LocalFS transport; plumbing commands default to
			// runqd.sock, so the preset templates are verbatim portable.
			synth, serr := synthLocalRunqdTarget(tc, dataDir)
			if serr != nil {
				return nil, fmt.Errorf("target %q: %w", tc.Name, serr)
			}
			ensureRunqd(logger, dataDir)
			be, berr = backend.NewSSHBackend(backend.SSHBackendConfig{
				Target: synth, Store: st, GlobalCfg: storageCfg, Logger: logger,
				FS: rfs.NewLocalFS(),
			})
			if berr == nil {
				logger.Info("localhost runqd lane registered", "target", tc.Name)
			}
		}
		if berr != nil {
			return nil, fmt.Errorf("build lane for target %q: %w", tc.Name, berr)
		}
		targets[tc.Name] = be
		lanes = append(lanes, be)
	}

	multiBe, err := backend.NewMultiBackend(targets, st, defaultTarget)
	if err != nil {
		return nil, fmt.Errorf("build multi-backend: %w", err)
	}

	d := &Daemon{
		Store:       st,
		Logger:      logger,
		sshBackends: lanes,
		pidPath:     paths.PIDPath,
		socketPath:  paths.SocketPath,
	}

	// The dashboard mux is the client's ONLY server surface: always built
	// (the CLI socket needs it); TCP listening is what dashboard.enabled
	// gates.
	d.Dashboard = dashboard.NewServer(multiBe, storageCfg)
	dashCfg := storageCfg.Dashboard
	if dashCfg == nil || dashCfg.Enabled {
		listen := "127.0.0.1:8077"
		if dashCfg != nil && dashCfg.Listen != "" {
			listen = dashCfg.Listen
		}
		d.dashboardListen = listen
		logger.Info("dashboard enabled", "listen", listen)
	}
	return d, nil
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
