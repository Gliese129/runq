package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gliese129/runq/internal/backend"
	"github.com/spf13/cobra"
)

// ── runq results — recorded results (runq.record) of a job ──
//
// Default view: one row per series (identity group), showing each series'
// LATEST record (mode one of the alignment: latest + honest annotation —
// rows whose x lags the global maximum get an explicit @<x> tag). No best
// highlighting: direction is unknowable server-side and the CLI's main
// consumer is agents; humans compare in the web UI.

var resultsCmd = &cobra.Command{
	Use:   "results <job_id>",
	Short: "Show results recorded via runq.record for a job",
	Example: `  runq results abc123
  runq results abc123 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runResults,
}

func runResults(cmd *cobra.Command, args []string) error {
	return withBackend(cmd, func(be backend.Backend) error {
		ctx := cmd.Context()
		// Refresh for poll-model backends; push-model backends no-op.
		_ = be.RefreshJob(ctx, args[0])

		res, err := be.JobResults(ctx, args[0])
		if err != nil {
			return err
		}
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			printJSON(res) // wire verbatim — no display adapter
			return nil
		}
		renderResultsTable(res)
		return nil
	})
}

func renderResultsTable(res *backend.JobResults) {
	if res.N == 0 {
		fmt.Println("no recorded results — emit them with runq.record(metrics, **axes) in your script")
		return
	}

	// Column plan: identity, label axes (sorted), metrics, lag annotation.
	labelAxes := []string{}
	for name, ax := range res.Schema.Axes {
		if ax.Role == "label" {
			labelAxes = append(labelAxes, name)
		}
	}
	sort.Strings(labelAxes)

	identityHeader := "SERIES"
	if _, ok := res.Schema.Axes["model"]; ok {
		identityHeader = "MODEL"
	}
	primaryX := ""
	if len(res.Schema.XAxes) > 0 {
		primaryX = res.Schema.XAxes[0]
	}

	// Latest record per group (see latestIdx: x-based, off-axis records
	// excluded). Global max x defines "caught up".
	lastIdx := make([]int, len(res.Schema.Groups))
	globalMaxX, haveX := 0.0, false
	for gi, g := range res.Schema.Groups {
		lastIdx[gi] = latestIdx(res, g, primaryX)
		if primaryX != "" {
			if x, ok := toFloatCell(res.Cols.Axes[primaryX][lastIdx[gi]]); ok {
				if !haveX || x > globalMaxX {
					globalMaxX, haveX = x, true
				}
			}
		}
	}

	w := newTable()
	header := []string{identityHeader}
	header = append(header, upperAll(labelAxes)...)
	header = append(header, upperAll(res.Schema.Metrics)...)
	header = append(header, "") // lag annotation
	fmt.Fprintln(w, strings.Join(header, "\t"))

	for gi, g := range res.Schema.Groups {
		i := lastIdx[gi]
		cells := []string{g.Key}
		for _, name := range labelAxes {
			cells = append(cells, axisCell(res.Schema.Axes[name], res.Cols.Axes[name][i]))
		}
		for _, m := range res.Schema.Metrics {
			cells = append(cells, metricCell(res.Cols.Metrics[m][i]))
		}
		annot := ""
		if haveX {
			if x, ok := toFloatCell(res.Cols.Axes[primaryX][i]); ok && x < globalMaxX {
				annot = "@" + primaryX + " " + formatNum(x)
			}
		}
		cells = append(cells, annot)
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()

	if res.Truncated {
		fmt.Printf("! %d records dropped by the per-task ingest cap — the table above is incomplete\n", res.Skipped)
	}
	for name, ax := range res.Schema.Axes {
		if ax.Nulled > 0 {
			fmt.Printf("! axis %q: %d values had conflicting types and were nulled\n", name, ax.Nulled)
		}
	}
}

// latestIdx picks a group's "latest" record. x-based slices operate on
// the group's x-bearing records only — the wire sorts them into a
// monotonic prefix, with OFF-AXIS records (no primary x) as the tail —
// so latest = the last x-bearing record, found by walking back over the
// null tail. A group with no x-bearing records (or no x axis at all)
// degrades to sequence order, i.e. its last record (ts-sorted).
func latestIdx(res *backend.JobResults, g backend.ResultRange, primaryX string) int {
	last := g.Offset + g.Count - 1
	if primaryX == "" {
		return last
	}
	col := res.Cols.Axes[primaryX]
	for i := last; i >= g.Offset; i-- {
		if _, ok := toFloatCell(col[i]); ok {
			return i
		}
	}
	return last
}

// axisCell renders one axis value: vocab lookup for str, plain formats
// otherwise. Vocab indices arrive as int in-process and float64 after the
// proxy's JSON round-trip — accept both.
func axisCell(ax backend.ResultAxis, v any) string {
	if v == nil {
		return "—"
	}
	switch ax.Type {
	case "str":
		idx := -1
		switch t := v.(type) {
		case int:
			idx = t
		case float64:
			idx = int(t)
		}
		if idx >= 0 && idx < len(ax.Vocab) {
			return ax.Vocab[idx]
		}
		return "?"
	case "num":
		if f, ok := toFloatCell(v); ok {
			return formatNum(f)
		}
	case "bool":
		if b, ok := v.(bool); ok {
			return strconv.FormatBool(b)
		}
	}
	return fmt.Sprint(v)
}

func metricCell(v *float64) string {
	if v == nil {
		return "—"
	}
	return formatNum(*v)
}

func toFloatCell(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

func formatNum(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func upperAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func init() {
	resultsCmd.Flags().Bool("json", false, "output the wire response verbatim")
	resultsCmd.Flags().StringP("target", "t", "", "Compute target (for target-scoped resolution)")

	resultsCmd.GroupID = groupCore
	rootCmd.AddCommand(resultsCmd)
}
