package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/workspace"
	"github.com/spf13/cobra"
)

// ── runq metrics-index（hidden plumbing）──
//
// 在 metrics.jsonl 所在的机器上构建 metrics.pyr 多分辨率索引。调用方是
// run.sh 的收尾（done marker 之前，`"$RUNQ_BIN" metrics-index build ... ||
// true` —— 索引失败不得污染任务终态）。纯本地 IO：这正是它作为 CLI 子命令
// 而非 daemon 功能存在的理由 —— 构建者必须与文件同侧。

var metricsIndexCmd = &cobra.Command{
	Use:    "metrics-index",
	Hidden: true,
	Short:  "Build the on-target metrics pyramid index (plumbing)",
}

var metricsIndexBuildCmd = &cobra.Command{
	Use:   "build --task-dir <dir>",
	Short: "Build metrics.pyr next to metrics.jsonl (single streaming pass)",
	RunE: func(cmd *cobra.Command, args []string) error {
		taskDir, _ := cmd.Flags().GetString("task-dir")
		if taskDir == "" {
			return fmt.Errorf("--task-dir is required")
		}
		// nil FS = this machine's filesystem — the whole point of running
		// the builder where the file lives.
		stats, err := workspace.BuildPyramid(cmd.Context(), nil, taskDir)
		if err != nil {
			return err
		}
		// Full accounting on stderr: dropped lines must be visible, not
		// silent (bare-NaN lines from Python's json.dumps land here).
		fmt.Fprintf(cmd.ErrOrStderr(), "pyramid: %d keys, %d points, %d other events, %d skipped lines\n",
			stats.Keys, stats.Points, stats.OtherEvents, stats.SkippedLines)
		return nil
	},
}

// inspect: the human window into the binary sidecar — header shape per
// key, and with --key a bucket table (full range, budgeted) so nobody
// ever needs a hex dump.
var metricsIndexInspectCmd = &cobra.Command{
	Use:   "inspect --task-dir <dir> [--key <k>]",
	Short: "Print a metrics.pyr header (and one key's buckets)",
	RunE: func(cmd *cobra.Command, args []string) error {
		taskDir, _ := cmd.Flags().GetString("task-dir")
		if taskDir == "" {
			return fmt.Errorf("--task-dir is required")
		}
		key, _ := cmd.Flags().GetString("key")
		jsonOut, _ := cmd.Flags().GetBool("json")

		infos, err := workspace.InspectPyramid(cmd.Context(), nil, taskDir)
		if err != nil {
			return err
		}
		if key == "" {
			if jsonOut {
				printJSON(infos)
				return nil
			}
			w := newTable()
			fmt.Fprintf(w, "KEY\tPOINTS\tLAYERS\t(records per layer, coarsest→leaf)\n")
			for _, in := range infos {
				fmt.Fprintf(w, "%s\t%d\t%d\t%v\n", in.Key, in.PointCount, len(in.LayerCounts), in.LayerCounts)
			}
			return w.Flush()
		}

		maxBuckets, _ := cmd.Flags().GetInt("buckets")
		buckets, err := workspace.QueryPyramid(cmd.Context(), nil, taskDir, key, 0, 0, maxBuckets)
		if err != nil {
			return err
		}
		if jsonOut {
			printJSON(buckets)
			return nil
		}
		w := newTable()
		fmt.Fprintf(w, "MIN\tMAX\tAVG\tSTD\tCOUNT\tNAN\tSTEPS\tFIRST_TS\tLAST_TS\tRAW_RANGE\n")
		for _, b := range buckets {
			fmt.Fprintf(w, "%.6g\t%.6g\t%.6g\t%.4g\t%d\t%d\t%d-%d\t%d\t%d\t%d-%d\n",
				b.Min, b.Max, b.Avg(), b.Std(), b.Count, b.NaNCount,
				b.FirstStep, b.LastStep, b.FirstTS, b.LastTS, b.RawStart, b.RawEnd)
		}
		return w.Flush()
	},
}

func init() {
	metricsIndexBuildCmd.Flags().String("task-dir", "", "task directory containing metrics.jsonl")
	metricsIndexInspectCmd.Flags().String("task-dir", "", "task directory containing metrics.pyr")
	metricsIndexInspectCmd.Flags().String("key", "", "dump this key's buckets (full range)")
	metricsIndexInspectCmd.Flags().Int("buckets", 40, "max buckets to dump with --key")
	metricsIndexInspectCmd.Flags().Bool("json", false, "raw JSON output")
	metricsIndexCmd.AddCommand(metricsIndexBuildCmd, metricsIndexInspectCmd)
	rootCmd.AddCommand(metricsIndexCmd)
}
