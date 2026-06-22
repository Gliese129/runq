package cli

import (
	"context"
	"fmt"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/utils"
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
	return withBackend(func(b backend.Backend) error {
		jobs, err := b.ListJobs(context.Background(), "")
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
	return withBackend(func(b backend.Backend) error {
		detail, err := b.GetJob(context.Background(), args[0])
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
	return withBackend(func(b backend.Backend) error {
		if err := b.KillJob(context.Background(), jobID); err != nil {
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
	return withBackend(func(b backend.Backend) error {
		if err := b.PauseJob(context.Background(), jobID); err != nil {
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
	return withBackend(func(b backend.Backend) error {
		if err := b.ResumeJob(context.Background(), jobID); err != nil {
			return err
		}
		fmt.Printf("job %s resumed\n", utils.IDColor(jobID))
		return nil
	})
}

var jobRmCmd = &cobra.Command{
	Use:     "rm <job_id>",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a completed job record",
	Args:    cobra.ExactArgs(1),
	RunE:    runJobRm,
}

// runJobRm stays on the daemon API: removing a completed job record is not
// part of the backend.Backend contract (the WebUI has no such action), so
// there is no mode-aware path to route through. Daemon-only by design.
func runJobRm(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	var resp map[string]any
	if err := doAndDecode("POST", "/api/jobs/"+jobID+"/rm", nil, &resp); err != nil {
		return err
	}
	fmt.Printf("job %s removed\n", utils.IDColor(jobID))
	return nil
}

func init() {
	jobCmd.AddCommand(jobLsCmd)
	jobCmd.AddCommand(jobShowCmd)
	jobCmd.AddCommand(jobKillCmd)
	jobCmd.AddCommand(jobPauseCmd)
	jobCmd.AddCommand(jobResumeCmd)
	jobCmd.AddCommand(jobRmCmd)
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
	RunE:  func(cmd *cobra.Command, args []string) error { return runJobArchive(args[0], true) },
}

var jobUnarchiveCmd = &cobra.Command{
	Use:   "unarchive <job_id>",
	Short: "Bring an archived job back to the default lists",
	Args:  cobra.ExactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runJobArchive(args[0], false) },
}

func runJobArchive(jobID string, archive bool) error {
	verb := "archived"
	if !archive {
		verb = "unarchived"
	}
	if _, mode, err := loadModeConfig(); err == nil && mode == config.ModeHPC {
		_, st, err := newHPCBackend()
		if err != nil {
			return err
		}
		defer st.Close()
		op := st.UnarchiveJob
		if archive {
			op = st.ArchiveJob
		}
		if err := op(context.Background(), jobID); err != nil {
			return err
		}
		fmt.Printf("job %s %s\n", utils.IDColor(jobID), verb)
		return nil
	}
	path := "/api/jobs/" + jobID + "/unarchive"
	if archive {
		path = "/api/jobs/" + jobID + "/archive"
	}
	var resp map[string]any
	if err := doAndDecode("POST", path, nil, &resp); err != nil {
		return err
	}
	fmt.Printf("job %s %s\n", utils.IDColor(jobID), verb)
	return nil
}
