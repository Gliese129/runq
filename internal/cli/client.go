package cli

import (
	"encoding/json"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/gliese129/runq-lab/internal/api"
)

func getSocketPath() string {
	if socketPathFlags, _ := rootCmd.PersistentFlags().GetString("socket"); socketPathFlags != "" {
		return socketPathFlags
	}
	if socketPathEnv := os.Getenv("RUNQ_SOCKET"); socketPathEnv != "" {
		return socketPathEnv
	}
	return api.DefaultSocketPath()
}

// getPlumbingSocketPath resolves the socket for PLUMBING commands
// (sbatch/squeue/scancel, gpu --json, thaw, status): they address "this
// machine's executor" — runqd — not the client daemon. Same override
// precedence as getSocketPath; only the default differs. This split is what
// makes runq-preset templates portable verbatim (remote lab server or the
// client's own machine) and immune to the client-asks-itself loop.
func getPlumbingSocketPath() string {
	if socketPathFlags, _ := rootCmd.PersistentFlags().GetString("socket"); socketPathFlags != "" {
		return socketPathFlags
	}
	if socketPathEnv := os.Getenv("RUNQ_SOCKET"); socketPathEnv != "" {
		return socketPathEnv
	}
	return api.DefaultRunqdSocketPath()
}

// ── Helpers (provided) ──

// printJSON prints v as indented JSON to stdout.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// newTable creates a tab-aligned writer for CLI table output.
// Call w.Flush() after writing all rows.
func newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

// humanBytes formats a byte count with a human-readable suffix.
// Negative values (e.g. stat failed → -1) render as "unknown".
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}
	const (
		_  = iota
		KB = 1 << (iota * 10)
		MB
		GB
		TB
		PB
	)
	switch {
	case n >= PB:
		return strconv.FormatFloat(float64(n)/float64(PB), 'f', 1, 64) + " PB"
	case n >= TB:
		return strconv.FormatFloat(float64(n)/float64(TB), 'f', 1, 64) + " TB"
	case n >= GB:
		return strconv.FormatFloat(float64(n)/float64(GB), 'f', 1, 64) + " GB"
	case n >= MB:
		return strconv.FormatFloat(float64(n)/float64(MB), 'f', 1, 64) + " MB"
	case n >= KB:
		return strconv.FormatFloat(float64(n)/float64(KB), 'f', 1, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}
