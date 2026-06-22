package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var pauseCmd = &cobra.Command{
	Use:   "pause <job_id>",
	Short: "Pause a job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runJobAction(cmd, args[0], "pause")
	},
}

var resumeCmd = &cobra.Command{
	Use:   "resume <job_id>",
	Short: "Resume a paused job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runJobAction(cmd, args[0], "resume")
	},
}

func init() {
	pauseCmd.Flags().Bool("json", false, "output raw JSON")
	resumeCmd.Flags().Bool("json", false, "output raw JSON")
	pauseCmd.GroupID = groupCore
	resumeCmd.GroupID = groupCore
	rootCmd.AddCommand(pauseCmd, resumeCmd)
}

func runJobAction(cmd *cobra.Command, jobID, action string) error {
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	backend, closeBackend, err := newBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()

	switch action {
	case "pause":
		err = backend.PauseJob(context.Background(), jobID)
	case "resume":
		err = backend.ResumeJob(context.Background(), jobID)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		printJSON(map[string]bool{"ok": true})
		return nil
	}
	fmt.Printf("job %s %sd\n", jobID, action)
	return nil
}
