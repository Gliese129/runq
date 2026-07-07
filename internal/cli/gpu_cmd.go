package cli

import (
	"fmt"
	"maps"
	"slices"

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

	// Human view: aggregate across targets — {target: GPUSlot[]}. The API
	// itself is per-target (D11); aggregation happens client-side.
	return withBackend(cmd, func(be backend.Backend) error {
		p, ok := be.(*api.Proxy)
		if !ok {
			return fmt.Errorf("gpu status requires the daemon proxy")
		}
		byTarget, err := p.GPUStatusByTarget(cmd.Context())
		if err != nil {
			return err
		}
		if output, _ := cmd.Flags().GetString("output"); output == "json" {
			printJSON(byTarget) // {target: GPUSlot[]}
			return nil
		}

		names := slices.Sorted(maps.Keys(byTarget))
		for _, name := range names {
			gpus := byTarget[name]
			fmt.Printf("── %s ──\n", name)
			if len(gpus) == 0 {
				fmt.Println("  (no gpu info)")
				continue
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
		}
		return nil
	})
}

func init() {
	gpuCmd.GroupID = groupDiag
	gpuCmd.Flags().Bool("json", false, "Machine-readable output ([]GPUSlot JSON, THIS machine — plumbing)")
	gpuCmd.Flags().StringP("output", "o", "", "Output format (json: {target: GPUSlot[]})")
	rootCmd.AddCommand(gpuCmd)
}
