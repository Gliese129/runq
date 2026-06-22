package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// ── runq ps / ls (unified job list) ──

var psCmd = &cobra.Command{
	Use:     "ps",
	Aliases: []string{"ls"},
	Short:   "List jobs in the configured backend",
	Example: `  runq ps
  runq ls
  runq ps --json`,
	RunE: runPs,
}

func runPs(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	jsonOut, _ := cmd.Flags().GetBool("json")
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()
	jobs, err := backend.ListJobs(context.Background(), "")
	if err != nil {
		return err
	}
	if output == "json" || jsonOut {
		printJSON(jobs)
		return nil
	}
	return printDashboardJobs(jobs)
}

func init() {
	psCmd.Flags().StringP("output", "o", "", "Output format (json)")
	psCmd.Flags().Bool("json", false, "output raw JSON")

	psCmd.GroupID = groupCore
	rootCmd.AddCommand(psCmd)
}
