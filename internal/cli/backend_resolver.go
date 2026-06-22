package cli

import (
	"fmt"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

// loadModeConfig loads the global config and resolves the active mode.
func loadModeConfig() (*config.GlobalConfig, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	return cfg, config.ConfigMode(cfg), nil
}

// newBackend creates a mode-aware backend. Callers must invoke the returned
// closer when done (HPC opens a DB handle; daemon is stateless).
func newBackend(mode string) (backend.Backend, func(), error) {
	switch mode {
	case config.ModeHPC:
		return backend.NewHPCBackendFromConfig()
	case config.ModeDaemon:
		return backend.NewDaemonBackend(getSocketPath()), func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unsupported mode %q", mode)
	}
}

// withBackend resolves the configured mode, opens the mode-aware Backend, runs
// fn, and tears the backend down. This is the single read/control entry point
// user-facing CLI commands should use so daemon and HPC behave identically.
func withBackend(fn func(backend.Backend) error) error {
	_, mode, err := loadModeConfig()
	if err != nil {
		return err
	}
	be, closeBackend, err := newBackend(mode)
	if err != nil {
		return err
	}
	defer closeBackend()
	return fn(be)
}
