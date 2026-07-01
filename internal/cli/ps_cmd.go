package cli

import (
	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

// ── runq ps / ls (unified job list) ──

var psCmd = &cobra.Command{
	Use:     "ps",
	Aliases: []string{"ls"},
	Short:   "List jobs in the configured backend",
	Example: `  runq ps
  runq ls
  runq ps --json
  runq ps -t tsubame`,
	RunE: runPs,
}

func runPs(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	jsonOut, _ := cmd.Flags().GetBool("json")
	return withBackend(cmd, func(be backend.Backend) error {
		jobs, err := be.ListJobs(cmd.Context(), "")
		if err != nil {
			return err
		}
		if output == "json" || jsonOut {
			printJSON(jobs)
			return nil
		}
		return printDashboardJobs(jobs)
	})
}

func init() {
	psCmd.Flags().StringP("output", "o", "", "Output format (json)")
	psCmd.Flags().Bool("json", false, "output raw JSON")
	psCmd.Flags().StringP("target", "t", "", "Filter jobs by compute target")

	psCmd.GroupID = groupCore
	rootCmd.AddCommand(psCmd)
}
