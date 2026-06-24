package cli

import (
	"fmt"

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
	return withBackend(func(be backend.Backend) error {
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
	rootCmd.AddCommand(gpuCmd)
}
