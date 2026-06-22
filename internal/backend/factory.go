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
