package cli

import (
	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/backend"
)

// withBackend opens a DaemonBackend connected to the running daemon's Unix
// socket, runs fn, and tears down. This is the single entry point for all
// CLI commands that need a Backend — the daemon owns all targets (local GPU
// + remote SSH) and handles routing internally via MultiBackend.
func withBackend(fn func(backend.Backend) error) error {
	be := backend.NewDaemonBackend(api.NewClient(getSocketPath()))
	return fn(be)
}
