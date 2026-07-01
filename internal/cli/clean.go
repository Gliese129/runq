package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/utils"
	"github.com/spf13/cobra"
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove tasks and their artifacts based on selectors",
	Long: `Remove tasks matching the given selectors, their log files, and jobs
that have no remaining tasks.

Selectors (at least one required):
  --older-than <dur>   Tasks finished before this threshold
  --orphan             Tasks whose workspace directory is missing (DB-only)
  --archived           Tasks belonging to archived jobs
  --job <id>           All tasks in a specific job
  --task <id>          A specific task

Modifiers:
  --ckpt-only          Only delete checkpoints/ directory, keep other artifacts and DB records
  --show               Preview what would be deleted (non-interactive)
  --yes                Skip the confirmation prompt

Duration format: additive segments like 7d, 1m2w, 2w3d4h
  h = hours, d = days, w = weeks (7d), m = months (30d), y = years (365d)

Examples:
  runq clean --older-than 7d        # older than 7 days
  runq clean --orphan               # orphan tasks (no files on disk)
  runq clean --archived             # tasks from archived jobs
  runq clean --job <id>             # all tasks in a job
  runq clean --orphan --older-than 1m  # orphan tasks older than 30 days
  runq clean --older-than 7d --show    # preview only
  runq clean --job <id> --ckpt-only    # delete checkpoints only`,
	RunE: runClean,
}

func init() {
	cleanCmd.Flags().String("older-than", "", "Age threshold, e.g. 7d, 1m2w, 2w3d4h")
	cleanCmd.Flags().Bool("orphan", false, "Select orphan tasks (workspace directory missing)")
	cleanCmd.Flags().Bool("archived", false, "Select tasks from archived jobs")
	cleanCmd.Flags().String("job", "", "Select all tasks in a specific job")
	cleanCmd.Flags().String("task", "", "Select a specific task")
	cleanCmd.Flags().Bool("ckpt-only", false, "Only delete checkpoints/, keep other artifacts and DB records")
	cleanCmd.Flags().Bool("show", false, "Preview what would be deleted without actually deleting")
	cleanCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	cleanCmd.Flags().StringP("target", "t", "", "Scope clean to a specific compute target")

	cleanCmd.GroupID = groupDiag
	rootCmd.AddCommand(cleanCmd)
}

func buildCleanOptions(cmd *cobra.Command) (backend.CleanOptions, error) {
	var opts backend.CleanOptions

	olderThan, _ := cmd.Flags().GetString("older-than")
	if olderThan != "" {
		dur, err := utils.ParseHumanDuration(olderThan)
		if err != nil {
			return opts, err
		}
		cutoff := time.Now().Add(-dur)
		opts.OlderThan = &cutoff
	}

	opts.Orphan, _ = cmd.Flags().GetBool("orphan")
	opts.Archived, _ = cmd.Flags().GetBool("archived")
	opts.JobID, _ = cmd.Flags().GetString("job")
	opts.TaskID, _ = cmd.Flags().GetString("task")
	opts.CkptOnly, _ = cmd.Flags().GetBool("ckpt-only")
	showOnly, _ := cmd.Flags().GetBool("show")
	opts.DryRun = showOnly

	// At least one selector must be given.
	if !opts.Orphan && !opts.Archived && opts.JobID == "" && opts.TaskID == "" && opts.OlderThan == nil {
		return opts, fmt.Errorf("at least one selector required: --older-than, --orphan, --archived, --job, --task")
	}

	return opts, nil
}

func runClean(cmd *cobra.Command, args []string) error {
	opts, err := buildCleanOptions(cmd)
	if err != nil {
		return err
	}

	return withBackend(cmd, func(be backend.Backend) error {
		// Always preview first to show the user what will be cleaned.
		previewOpts := opts
		previewOpts.DryRun = true
		result, err := be.Clean(cmd.Context(), previewOpts)
		if err != nil {
			return err
		}

		if len(result.Preview) == 0 {
			fmt.Println("Nothing to clean.")
			return nil
		}

		// Display preview.
		printCleanPreview(result.Preview)

		// If --show, stop here.
		if opts.DryRun {
			return nil
		}

		// Interactive confirmation.
		yes, _ := cmd.Flags().GetBool("yes")
		if !yes {
			if !confirmClean(len(result.Preview)) {
				fmt.Println("Aborted.")
				return nil
			}
		}

		// Execute the real clean.
		result, err = be.Clean(cmd.Context(), opts)
		if err != nil {
			return err
		}

		fmt.Printf("Cleaned %d tasks, %d jobs", result.Tasks, result.Jobs)
		if result.FreedBytes > 0 {
			fmt.Printf(", freed %s", humanBytes(result.FreedBytes))
		}
		fmt.Println()
		return nil
	})
}

func printCleanPreview(items []backend.CleanPreviewItem) {
	fmt.Printf("Detected %d tasks:\n", len(items))
	for _, p := range items {
		var detail string
		switch p.Action {
		case backend.CleanActionDBOnly:
			detail = "no files"
		case backend.CleanActionCkpt:
			detail = "will clean checkpoints"
		case backend.CleanActionCkptDB:
			detail = "no checkpoints"
		default:
			detail = "will clean all files"
		}
		orphanTag := ""
		if p.Orphan {
			orphanTag = " [orphan]"
		}
		finished := ""
		if p.FinishedAt != nil {
			finished = " finished=" + time.Unix(*p.FinishedAt, 0).Format("2006-01-02 15:04")
		}
		fmt.Printf("  %s  %-8s  (%s)%s  reason=%s%s\n",
			p.TaskID[:min(8, len(p.TaskID))], p.Status, detail, orphanTag, p.Reason, finished)
	}
}

func confirmClean(count int) bool {
	fmt.Printf("Proceed with cleaning %d tasks? [y/N] ", count)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
