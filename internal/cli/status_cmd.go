package cli

import (
	"context"
	"fmt"

	"github.com/gliese129/runq/internal/config"
	"github.com/spf13/cobra"
)

// ── runq status ──

var statusCmd = &cobra.Command{
	Use:   "status [job_id]",
	Short: "Show daemon status, queue length, and scheduling info",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	if len(args) == 1 {
		_, mode, err := loadModeConfig()
		if err != nil {
			return err
		}
		backend, closeBackend, err := newBackend(mode)
		if err != nil {
			return err
		}
		defer closeBackend()
		detail, err := backend.GetJob(context.Background(), args[0])
		if err != nil {
			return err
		}
		if jsonOut {
			printJSON(detail)
			return nil
		}
		return printDashboardDetail(detail)
	}

	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	if mode == config.ModeHPC {
		return fmt.Errorf("status requires <job_id> in hpc mode")
	}
	var s map[string]any
	if err := doAndDecode("GET", "/api/status", nil, &s); err != nil {
		return err
	}
	if jsonOut {
		printJSON(s)
		return nil
	}

	fmt.Printf("Running:   %.0f\n", s["running"])
	fmt.Printf("Pending:   %.0f\n", s["pending"])
	fmt.Printf("GPUs free: %.0f\n", s["gpus_free"])
	return nil
}

func init() {
	statusCmd.Flags().Bool("json", false, "output raw JSON")

	statusCmd.GroupID = groupDiag
	rootCmd.AddCommand(statusCmd)
}
