package cli

import (
	"fmt"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/utils"
	"github.com/spf13/cobra"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage jobs (sweep submissions)",
}

var jobLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all jobs",
	RunE:    runJobLs,
}

// runJobLs lists jobs via the mode-aware backend (daemon socket or HPC store),
// the same path the WebUI and `runq ps` use — so the list reconciles and
// projects identically regardless of mode.
func runJobLs(cmd *cobra.Command, args []string) error {
	return withBackend(cmd, func(b backend.Backend) error {
		jobs, err := b.ListJobs(cmd.Context(), "")
		if err != nil {
			return err
		}
		return printDashboardJobs(jobs)
	})
}

var jobShowCmd = &cobra.Command{
	Use:   "show <job_id>",
	Short: "Show job details and its tasks",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobShow,
}

func runJobShow(cmd *cobra.Command, args []string) error {
	return withBackend(cmd, func(b backend.Backend) error {
		detail, err := b.GetJob(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		printJSON(detail)
		return nil
	})
}

var jobKillCmd = &cobra.Command{
	Use:   "kill <job_id>",
	Short: "Kill all tasks in a job",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobKill,
}

func runJobKill(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	return withBackend(cmd, func(b backend.Backend) error {
		if err := b.KillJob(cmd.Context(), jobID); err != nil {
			return err
		}
		fmt.Printf("job %s killed\n", utils.IDColor(jobID))
		return nil
	})
}

var jobPauseCmd = &cobra.Command{
	Use:   "pause <job_id>",
	Short: "Pause a job (stop dispatching new tasks, running tasks continue)",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobPause,
}

func runJobPause(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	return withBackend(cmd, func(b backend.Backend) error {
		if err := b.PauseJob(cmd.Context(), jobID); err != nil {
			return err
		}
		fmt.Printf("job %s paused\n", utils.IDColor(jobID))
		return nil
	})
}

var jobResumeCmd = &cobra.Command{
	Use:   "resume <job_id>",
	Short: "Resume a paused job",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobResume,
}

func runJobResume(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	return withBackend(cmd, func(b backend.Backend) error {
		if err := b.ResumeJob(cmd.Context(), jobID); err != nil {
			return err
		}
		fmt.Printf("job %s resumed\n", utils.IDColor(jobID))
		return nil
	})
}

func init() {
	jobLsCmd.Flags().StringP("target", "t", "", "Filter jobs by compute target")
	jobCmd.AddCommand(jobLsCmd)
	jobCmd.AddCommand(jobShowCmd)
	jobCmd.AddCommand(jobKillCmd)
	jobCmd.AddCommand(jobPauseCmd)
	jobCmd.AddCommand(jobResumeCmd)
	jobCmd.AddCommand(jobArchiveCmd)
	jobCmd.AddCommand(jobUnarchiveCmd)
	jobCmd.GroupID = groupManagement
	rootCmd.AddCommand(jobCmd)
}

// ── archive / unarchive (mode-aware: daemon API or HPC store) ──

var jobArchiveCmd = &cobra.Command{
	Use:   "archive <job_id>",
	Short: "Hide a job from default lists (data and workspace untouched; reversible)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runJobArchive(cmd, args[0], true)
	},
}

var jobUnarchiveCmd = &cobra.Command{
	Use:   "unarchive <job_id>",
	Short: "Bring an archived job back to the default lists",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runJobArchive(cmd, args[0], false)
	},
}

func runJobArchive(cmd *cobra.Command, jobID string, archive bool) error {
	verb := "archived"
	if !archive {
		verb = "unarchived"
	}
	return withBackend(cmd, func(be backend.Backend) error {
		var err error
		if archive {
			err = be.ArchiveJob(cmd.Context(), jobID)
		} else {
			err = be.UnarchiveJob(cmd.Context(), jobID)
		}
		if err != nil {
			return err
		}
		fmt.Printf("job %s %s\n", utils.IDColor(jobID), verb)
		return nil
	})
}
