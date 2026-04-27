package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gliese129/runq/internal/store"
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
}

func runClean(cmd *cobra.Command, args []string) error {
	olderThan, _ := cmd.Flags().GetString("older-than")
	showOnly, _ := cmd.Flags().GetBool("show")

	dur, err := utils.ParseHumanDuration(olderThan)
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-dur)

	// Open DB directly — clean works without daemon running.
	dbPath := store.DefaultDBPath()
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	ctx := context.Background()

	tasks, err := st.ListFinishedTasksBefore(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("query tasks: %w", err)
	}

	if len(tasks) == 0 {
		fmt.Println("Nothing to clean.")
		return nil
	}

	if showOnly {
		fmt.Printf("Would clean %d tasks (finished before %s):\n", len(tasks), cutoff.Format("2006-01-02 15:04"))
		for _, t := range tasks {
			finished := ""
			if t.FinishedAt != nil {
				finished = t.FinishedAt.Format("2006-01-02 15:04")
			}
			fmt.Printf("  %s  %-8s  finished=%s  log=%s\n", t.ID, t.Status, finished, t.LogPath)
		}
		return nil
	}

	// Delete log files and task records.
	var deletedTasks int
	var freedBytes int64
	for _, t := range tasks {
		freed := cleanTaskArtifacts(t)
		freedBytes += freed

		if err := st.DeleteTask(ctx, t.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete task %s: %v\n", t.ID, err)
			continue
		}
		deletedTasks++
	}

	// Delete orphan jobs (done jobs with no remaining tasks).
	deletedJobs, err := st.DeleteOrphanJobs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to clean orphan jobs: %v\n", err)
	}

	fmt.Printf("Cleaned %d tasks, %d jobs", deletedTasks, deletedJobs)
	if freedBytes > 0 {
		fmt.Printf(", freed %s", formatBytes(freedBytes))
	}
	fmt.Println()
	return nil
}

// cleanTaskArtifacts deletes a finished task's log file.
// Returns the number of bytes freed.
// TODO(L3): also remove checkpoint_dir when checkpoint management lands.
func cleanTaskArtifacts(t store.TaskRow) int64 {
	if t.LogPath == "" {
		return 0
	}
	info, err := os.Stat(t.LogPath)
	if err != nil {
		return 0
	}
	size := info.Size()
	if err := os.Remove(t.LogPath); err != nil {
		return 0
	}
	return size
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
