package cli

import (
	"encoding/json"
	"os"
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
