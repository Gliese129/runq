package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/executor"
	"github.com/gliese129/runq/internal/gpu"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the runq daemon process",
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the scheduler daemon",
	Example: `  runq daemon start
  RUNQ_DATA_DIR=/data/runq runq daemon start`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Resolve data directory: RUNQ_DATA_DIR env > euid auto-detect.
		_, dataDir := utils.ResolveDataDir()
		paths := utils.PathsFromDataDir(dataDir)

		// Ensure data and log directories exist.
		if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
			return fmt.Errorf("create data dir: %w", err)
		}

		// Open DB (schema migration runs automatically).
		s, err := store.Open(paths.DBPath)
		if err != nil {
			return err
		}

		gpus, err := gpu.Detect()
		if err != nil {
			return err
		}

		pool := scheduler.NewGPUPool(gpus)
		cfg := scheduler.DefaultConfig()
		queue := scheduler.NewQueue()
		exec_ := executor.New()
		logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logger.Info("daemon starting", "data_dir", dataDir, "db", paths.DBPath, "socket", paths.SocketPath)

		deps := api.Deps{
			Store:     s,
			Registry:  project.NewRegistry(s.DB()),
			Scheduler: scheduler.New(cfg, queue, pool, exec_, s, logger),
			Queue:     queue,
			Pool:      pool,
			Executor:  exec_,
			Logger:    logger,
		}
		server := api.NewServer(deps, paths.SocketPath, paths.PIDPath)

		deps.Scheduler.Start()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			logger.Info("shutdown signal received")
			deps.Scheduler.Shutdown()
			if err := server.Shutdown(); err != nil {
				logger.Error("server shutdown failed", "error", err)
			}
			if err := s.Close(); err != nil {
				logger.Error("db close failed", "error", err)
			}
		}()

		if err := server.Start(); err != nil {
			return err
		}
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the scheduler daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		pid, startTime, err := api.ReadPID(api.DefaultPIDPath())
		if err != nil {
			return err
		}
		if pid == 0 {
			return fmt.Errorf("daemon is not running")
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return fmt.Errorf("process no response in 10s")
			case <-ticker.C:
				if !utils.IsProcessAlive(pid, startTime) {
					return nil
				}
			}

		}
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the scheduler daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Stopping daemon...")
		if err := daemonStopCmd.RunE(cmd, args); err != nil {
			return err
		}
		fmt.Println("Starting daemon...")
		return daemonStartCmd.RunE(cmd, args)
	},
}

func init() {
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
