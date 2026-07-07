package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

// ── runq gpu ──

var gpuCmd = &cobra.Command{
	Use:   "gpu",
	Short: "Show GPU allocation status",
	RunE:  runGPU,
}

func runGPU(cmd *cobra.Command, args []string) error {
	// --json is the machine contract consumed by the runq preset's
	// gpu_template: "THIS machine's GPUs" — plumbing socket (runqd), so the
	// template is portable verbatim and loop-proof. The human view (no
	// --json) goes through the client daemon: aggregated across all targets.
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		p := api.NewProxy(getPlumbingSocketPath())
		gpus, err := p.MachineGPUStatus(cmd.Context())
		if err != nil {
			return err
		}
		printJSON(gpus)
		return nil
	}

	return withBackend(cmd, func(be backend.Backend) error {
		gpus, err := be.GPUStatus(cmd.Context())
		if err != nil {
			return err
		}

		w := newTable()
		fmt.Fprintf(w, "GPU\tNAME\tMEM\tUTIL\tTASK\n")
		for _, g := range gpus {
			task := "-"
			if g.TaskID != "" {
				task = g.TaskID
			}
			mem := fmt.Sprintf("%d/%d MB", g.MemUsedMB, g.MemTotalMB)
			fmt.Fprintf(w, "%d\t%s\t%s\t%d%%\t%s\n", g.Index, g.Name, mem, g.UtilPercent, task)
		}
		w.Flush()
		return nil
	})
}

func init() {
	gpuCmd.GroupID = groupDiag
	gpuCmd.Flags().Bool("json", false, "Machine-readable output ([]GPUSlot JSON)")
	rootCmd.AddCommand(gpuCmd)
}
