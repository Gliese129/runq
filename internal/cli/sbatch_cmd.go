package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
)

// The sbatch/squeue/scancel trio is the server side of the "runq preset":
// a remote runq client drives THIS machine's runqd exactly like it
// drives Slurm — submit one task, poll a status list, cancel by id. They are
// plumbing commands (hidden from help): humans use submit/ps/kill; these
// exist so the client's remote lane can use its ordinary command templates:
//
//	submit_template:      "runq sbatch {{run_sh}} --gpus {{gpus}} --task-dir {{task_dir}} --name {{name}}"
//	submit_id_regex:      "submitted (\\S+)"
//	status_list_template: "runq squeue"
//	kill_template:        "runq scancel {{ext_id}}"
var sbatchCmd = &cobra.Command{
	Use:    "sbatch <run_sh>",
	Short:  "Enqueue one pre-planned foreign task (runq preset plumbing)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gpus, _ := cmd.Flags().GetInt("gpus")
		name, _ := cmd.Flags().GetString("name")
		taskDir, _ := cmd.Flags().GetString("task-dir")
		logPath, _ := cmd.Flags().GetString("log")
		projectLabel, _ := cmd.Flags().GetString("project")

		p := api.NewProxy(getPlumbingSocketPath())
		id, err := p.Sbatch(cmd.Context(), backend.TaskSpec{
			RunSH: args[0], GPUs: gpus, Name: name,
			TaskDir: taskDir, LogPath: logPath, Project: projectLabel,
			// RQ-69: the client injects its per-attempt cancel handle via
			// env; adopting it as this server's task id makes external_id
			// deterministic (known before the submit response arrives).
			Handle: os.Getenv("RUNQ_SUBMIT_HANDLE"),
		})
		if err != nil {
			return err
		}
		// The one line the client's submit_id_regex parses.
		fmt.Printf("submitted %s\n", id)
		return nil
	},
}

var squeueCmd = &cobra.Command{
	Use:    "squeue",
	Short:  "List non-terminal tasks, one `<id> <STATUS>` per line (runq preset plumbing)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p := api.NewProxy(getPlumbingSocketPath())
		entries, err := p.Squeue(cmd.Context())
		if err != nil {
			return err
		}
		// Plain `<ext_id> <STATE>` lines — exactly what the client's batch
		// probe parser consumes. Uppercased: runq's status vocabulary IS
		// remote.ParseSignal's canonical vocabulary.
		for _, e := range entries {
			fmt.Printf("%s %s\n", e.ID, strings.ToUpper(e.Status))
		}
		return nil
	},
}

var scancelCmd = &cobra.Command{
	Use:    "scancel <task_id>",
	Short:  "Cancel one task by id (runq preset plumbing)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Plumbing socket: cancel on THIS machine's executor.
		p := api.NewProxy(getPlumbingSocketPath())
		return p.MachineKillTask(cmd.Context(), args[0])
	},
}

func init() {
	sbatchCmd.Flags().Int("gpus", 1, "GPUs this task needs on this server")
	sbatchCmd.Flags().String("name", "", "Display name (job note)")
	sbatchCmd.Flags().String("task-dir", "", "Task workspace dir prepared by the client (required)")
	sbatchCmd.Flags().String("log", "", "Log path (default <task-dir>/<task_id>.log)")
	sbatchCmd.Flags().String("project", "", "Client-side project name (plain label, no registration)")
	_ = sbatchCmd.MarkFlagRequired("task-dir")
	rootCmd.AddCommand(sbatchCmd, squeueCmd, scancelCmd)
}
