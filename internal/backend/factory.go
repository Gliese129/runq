package backend

import (
	"fmt"
	"os"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/hpc"
	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/store"
)

// NewHPCBackendFromConfig loads configs, opens the store, and returns a
// ready Backend. The returned closer must be deferred. Encapsulates all
// HPC wiring so callers never import hpc/hpcconfig/store directly.
func NewHPCBackendFromConfig() (Backend, func(), error) {
	globalCfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	hpcCfg, err := hpcconfig.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(hpcconfig.DataDir(), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.Open(hpcconfig.DBPath())
	if err != nil {
		return nil, nil, err
	}
	hpcBe := hpc.New(hpcCfg, st, globalCfg)
	return NewHPCBackend(hpcBe, st), func() { _ = st.Close() }, nil
}

// NewMultiBackendFromConfig builds a MultiBackend from the global config's
// targets[] section. Each target is wired to its appropriate backend:
//   - local (gpus)    → DaemonBackend via the provided daemonClient
//   - HPC (scheduler) → HPCBackend with shared store
//
// daemonClient satisfies the httpDoer interface (e.g. api.NewClient(socketPath)).
// Falls back to legacy mode-based construction when targets[] is empty.
func NewMultiBackendFromConfig(daemonClient httpDoer) (Backend, func(), error) {
	globalCfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}

	targets := globalCfg.ResolveTargets()
	defaultTarget := globalCfg.ResolveDefaultTarget()

	// Legacy path: no targets[] configured → single backend from Mode.
	if len(globalCfg.Targets) == 0 {
		return newLegacyBackend(globalCfg, daemonClient)
	}

	// Open shared store for all HPC targets.
	if err := os.MkdirAll(config.ConfigDir(), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create config dir: %w", err)
	}
	st, err := store.Open(config.DBPath())
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}

	beMap := make(map[string]Backend, len(targets))
	for _, t := range targets {
		switch t.Type() {
		case config.TargetTypeLocal:
			beMap[t.Name] = NewDaemonBackend(daemonClient)
		case config.TargetTypeHPC:
			hpcCfg, err := hpcconfig.Load()
			if err != nil {
				_ = st.Close()
				return nil, nil, fmt.Errorf("target %q: load hpc config: %w", t.Name, err)
			}
			hpcBe := hpc.New(hpcCfg, st, globalCfg)
			beMap[t.Name] = NewHPCBackend(hpcBe, st)
		default:
			_ = st.Close()
			return nil, nil, fmt.Errorf("target %q: unsupported type", t.Name)
		}
	}

	multi, err := NewMultiBackend(beMap, st, defaultTarget)
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return multi, func() { _ = st.Close() }, nil
}

// newLegacyBackend handles the old mode-based config (no targets[]).
func newLegacyBackend(cfg *config.GlobalConfig, daemonClient httpDoer) (Backend, func(), error) {
	switch config.ConfigMode(cfg) {
	case config.ModeHPC:
		return NewHPCBackendFromConfig()
	default:
		return NewDaemonBackend(daemonClient), func() {}, nil
	}
}
