package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// thawCmd implements `runq thaw` — release a global disk-freeze that was
// triggered by a task's SDK reporting low disk space. Idempotent: safe to call
// even when nothing is frozen (prints "system is not frozen" and exits 0).
var thawCmd = &cobra.Command{
	Use:   "thaw",
	Short: "Release the global disk-freeze and resume SIGSTOP'd tasks",
	Long: `Release the global disk-freeze that was entered when a task reported
low disk space. All previously frozen tasks receive SIGCONT and the
scheduler resumes dispatching pending tasks.

Idempotent: returns success even if the system is not currently frozen.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var result struct {
			WasFrozen   bool     `json:"was_frozen"`
			ThawedTasks []string `json:"thawed_tasks"`
		}
		if err := doAndDecode("POST", "/api/thaw", nil, &result); err != nil {
			return err
		}
		if !result.WasFrozen {
			fmt.Println("system is not frozen")
			return nil
		}
		fmt.Printf("thawed %d task(s)", len(result.ThawedTasks))
		if len(result.ThawedTasks) > 0 {
			fmt.Printf(": %v", result.ThawedTasks)
		}
		fmt.Println()
		return nil
	},
}

func init() {
	thawCmd.GroupID = groupManagement
	rootCmd.AddCommand(thawCmd)
}
