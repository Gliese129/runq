package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/spf13/cobra"
)

// withBackend opens a Proxy connected to the running daemon's Unix socket,
// resolves the active target (--target / $RUNQ_TARGET / .active-target),
// and runs fn. This is the single entry point for ALL CLI commands that need
// a Backend — every command automatically gets target resolution.
//
// The resolved target is applied as Proxy.TargetFilter, which:
//   - scopes list operations (ListJobs, ListArchivedJobs, Clean)
//   - serves as the default submit target when SubmitOptions.Target is empty
//   - is available for any future endpoint that benefits from target scoping
func withBackend(cmd *cobra.Command, fn func(backend.Backend) error) error {
	target := resolveTarget(cmd)
	be := api.NewProxy(getSocketPath())
	if target != "" {
		be.TargetFilter = target
	}
	return fn(be)
}

// resolveTarget reads the active target from the CLI --target flag, the
// RUNQ_TARGET env var, or the .active-target session file.
// Returns "" if no target is explicitly set (list = all targets,
// submit = daemon's default_target).
//
// Priority: --target flag > $RUNQ_TARGET > <configDir>/.active-target
func resolveTarget(cmd *cobra.Command) string {
	if cmd.Flags().Changed("target") {
		t, _ := cmd.Flags().GetString("target")
		return t
	}
	if t := os.Getenv("RUNQ_TARGET"); t != "" {
		return t
	}
	path := filepath.Join(config.ConfigDir(), ".active-target")
	if buf, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(buf)); t != "" {
			return t
		}
	}
	return ""
}
