package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove finished tasks, their logs, and empty jobs older than a threshold",
	Long: `Remove finished tasks (success/failed/killed), their log files, and jobs
that have no remaining tasks. Use --older-than to specify the age threshold.

Duration format: additive segments like 7d, 1m2w, 2w3d4h
  h = hours, d = days, w = weeks (7d), m = months (30d), y = years (365d)

Examples:
  runq clean --older-than 7d        # older than 7 days
  runq clean --older-than 1m        # older than 30 days
  runq clean --older-than 1m2w      # older than 44 days
  runq clean --older-than 7d --show # preview what would be deleted`,
	RunE: runClean,
}

func init() {
	cleanCmd.Flags().String("older-than", "", "Age threshold (required), e.g. 7d, 1m2w, 2w3d4h")
	cleanCmd.Flags().Bool("show", false, "Preview what would be deleted without actually deleting")
	cleanCmd.MarkFlagRequired("older-than")

	cleanCmd.GroupID = groupDiag
	rootCmd.AddCommand(cleanCmd)
}

func runClean(cmd *cobra.Command, args []string) error {
	olderThan, _ := cmd.Flags().GetString("older-than")
	showOnly, _ := cmd.Flags().GetBool("show")

	dur, err := utils.ParseHumanDuration(olderThan)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-dur)

	return withBackend(func(be backend.Backend) error {
		result, err := be.CleanOldTasks(context.Background(), cutoff, showOnly)
		if err != nil {
			return err
		}

		if showOnly {
			if len(result.Preview) == 0 {
				fmt.Println("Nothing to clean.")
				return nil
			}
			fmt.Printf("Would clean %d tasks (finished before %s):\n", len(result.Preview), cutoff.Format("2006-01-02 15:04"))
			for _, p := range result.Preview {
				finished := ""
				if p.FinishedAt != nil {
					finished = p.FinishedAt.Format("2006-01-02 15:04")
				}
				fmt.Printf("  %s  %-8s  finished=%s\n", p.TaskID, p.Status, finished)
			}
			return nil
		}

		fmt.Printf("Cleaned %d tasks, %d jobs", result.Tasks, result.Jobs)
		if result.FreedBytes > 0 {
			fmt.Printf(", freed %s", formatBytes(result.FreedBytes))
		}
		fmt.Println()
		return nil
	})
}

// formatBytes formats byte count into human-readable form.
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
