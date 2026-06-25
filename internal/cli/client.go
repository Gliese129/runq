package cli

import (
	"encoding/json"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/gliese129/runq/internal/api"
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
