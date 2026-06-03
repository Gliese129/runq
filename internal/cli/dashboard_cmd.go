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
	dashboardCmd.GroupID = groupCore
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(cmd *cobra.Command, args []string) error {
	cfg, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newDashboardBackend(mode)
	if err != nil {
		backend = dashboard.NewUnavailableBackend(err)
		closeBackend = func() {}
	}
	defer closeBackend()

	host, _ := cmd.Flags().GetString("host")
	port, _ := cmd.Flags().GetInt("port")
	if _, err := dashboard.ParsePort(strconv.Itoa(port)); err != nil {
		return err
	}

	server := dashboard.NewServer(backend, mode, cfg)
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

func newDashboardBackend(mode string) (dashboard.Backend, func(), error) {
	switch mode {
	case config.ModeHPC:
		b, st, err := newHPCBackend()
		if err != nil {
			return nil, nil, err
		}
		return dashboard.NewHPCBackend(b, st), func() { _ = st.Close() }, nil
	case config.ModeDaemon:
		return dashboard.NewDaemonBackend(getSocketPath(), nil), func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported mode %q", mode)
	}
}
