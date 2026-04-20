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
  runq daemon start --config ~/.runq/config.yaml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store.Open(store.DEFAULT_DB_PATH)
		if err != nil {
			return err
		}
		if err := s.Migrate(); err != nil {
			return err
		}
		gpus, err := gpu.Detect()
		if err != nil {
			return err
		}
		pool := scheduler.NewGPUPool(gpus)
		db := s.DB()
		cfg := scheduler.DefaultConfig()
		queue := scheduler.NewQueue()
		exec_ := executor.New()

		handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
		logger := slog.New(handler)
		deps := api.Deps{
			Store:     s,
			Registry:  project.NewRegistry(db),
			Scheduler: scheduler.New(cfg, queue, pool, exec_, logger),
			Queue:     queue,
			Pool:      pool,
			Executor:  exec_,
			Logger:    logger,
		}
		server := api.NewServer(deps, getSocketPath(), api.DefaultPIDPath())

		deps.Scheduler.Start()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			logger.Info("Received shutdown signal. Shutting down server...")
			deps.Scheduler.Shutdown()
			err := server.Shutdown()
			if err != nil {
				logger.Error("Could not shutdown server!")
			}
			err = s.Close()
			if err != nil {
				logger.Error("Could not close db!")
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
				if !api.IsProcessAlive(pid, startTime) {
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
