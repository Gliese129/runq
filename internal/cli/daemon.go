package cli

import (
	"fmt"

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
		fmt.Println("TODO: start daemon")
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the scheduler daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: stop daemon")
		return nil
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the scheduler daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("TODO: restart daemon")
		return nil
	},
}

func init() {
	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	rootCmd.AddCommand(daemonCmd)
}
