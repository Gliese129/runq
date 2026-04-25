package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/app"
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
		bg, _ := cmd.Flags().GetBool("detach")
		if bg {
			_, dataDir := utils.ResolveDataDir()
			logFile, _ := os.OpenFile(filepath.Join(dataDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
			subp := exec.Command(os.Args[0], "daemon", "start")
			subp.Stdin = nil
			subp.Stdout = logFile
			subp.Stderr = logFile
			subp.Dir = "/"                                     // set to root for background
			subp.Env = append(os.Environ(), "RUNQ_IS_CHILD=1") // maybe useful
			if err := subp.Start(); err != nil {
				return err
			}
			fmt.Printf("daemon started at backend with pid %d!", subp.Process.Pid)
			return nil
		}

		d, err := app.NewDaemon()
		if err != nil {
			return err
		}
		return d.Run()
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

		ctx, cancel := signal.NotifyContext(context.Background())
		defer cancel()
		deadline, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.Done():
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
	Short: "Restart the scheduler daemon (always starts in background)",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Stopping daemon...")
		if err := daemonStopCmd.RunE(cmd, args); err != nil {
			return err
		}
		// Force detach mode — restart should never block the terminal.
		fmt.Println("Starting daemon...")
		_ = daemonStartCmd.Flags().Set("detach", "true")
		return daemonStartCmd.RunE(daemonStartCmd, args)
	},
}

func init() {
	daemonStartCmd.Flags().BoolP("detach", "d", false, "run daemon in background")
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
