package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/dashboard"
	"github.com/spf13/cobra"
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Start the local dashboard HTTP server",
	RunE:  runDashboard,
}

func init() {
	dashboardCmd.Flags().String("host", "127.0.0.1", "host to bind")
	dashboardCmd.Flags().Int("port", 8077, "port to bind")
	dashboardCmd.Flags().String("assets-dir", "", "dashboard static assets directory")
	dashboardCmd.Flags().String("mode", "", "backend mode: daemon | hpc (default: config.yaml `mode:` key)")
	dashboardCmd.GroupID = groupCore
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(cmd *cobra.Command, args []string) error {
	cfg, mode, err := loadModeConfig()
	if err != nil {
		return err
	}

	// Precedence: --mode flag > config.yaml > default(daemon). Always state
	// the resolved mode AND its source — nobody should have to guess why
	// the dashboard came up in the wrong mode.
	modeSource := fmt.Sprintf("from %s", config.ConfigPath())
	if flagMode, _ := cmd.Flags().GetString("mode"); flagMode != "" {
		mode, err = config.NormalizeMode(flagMode)
		if err != nil {
			return err
		}
		modeSource = "from --mode flag"
	}
	fmt.Printf("mode: %s (%s; override with --mode)\n", mode, modeSource)

	be, closeBackend, err := newDashboardBackend(mode)
	if err != nil {
		be = backend.NewUnavailableBackend(err)
		closeBackend = func() {}
	}
	defer closeBackend()

	host, _ := cmd.Flags().GetString("host")
	port, _ := cmd.Flags().GetInt("port")
	assetsDir, _ := cmd.Flags().GetString("assets-dir")
	if _, err := dashboard.ParsePort(strconv.Itoa(port)); err != nil {
		return err
	}

	server := dashboard.NewServerWithAssets(be, mode, cfg, assetsDir)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: addr, Handler: server.Handler()}

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	fmt.Fprintf(cmd.OutOrStdout(), "Dashboard running at http://%s\n", addr)
	if server.StaticAssetsUnavailable() {
		fmt.Fprintf(cmd.OutOrStdout(), "Dashboard UI is not installed; API routes are still available. Build with -tags dashboard or pass --assets-dir.\n")
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		return nil
	case err := <-errCh:
		return err
	}
}

func loadModeConfig() (*config.GlobalConfig, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	return cfg, config.ConfigMode(cfg), nil
}

func newDashboardBackend(mode string) (backend.Backend, func(), error) {
	switch mode {
	case config.ModeHPC:
		b, st, err := newHPCBackend()
		if err != nil {
			return nil, nil, err
		}
		return backend.NewHPCBackend(b, st), func() { _ = st.Close() }, nil
	case config.ModeDaemon:
		return backend.NewDaemonBackend(getSocketPath()), func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported mode %q", mode)
	}
}

// withBackend resolves the configured mode, opens the mode-aware
// dashboard.Backend, runs fn, and tears the backend down. This is the single
// read/control entry point user-facing CLI commands should use so daemon and
// HPC behave identically (same reconciliation, capabilities, summary
// projection) — the same backend the WebUI goes through. Commands keep their
// own output formatting; only the data access goes through here.
func withBackend(fn func(backend.Backend) error) error {
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	be, closeBackend, err := newDashboardBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()
	return fn(be)
}
