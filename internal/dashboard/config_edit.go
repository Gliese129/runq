package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/rfs"
)

// ErrForwardRestartRequired is returned (wrapped) by the forward starter
// when the target has no lane in the running daemon — the one case where
// only a restart helps. Declared here so the hook's provider (app) and
// consumer (this handler) agree without an import cycle.
var ErrForwardRestartRequired = errors.New("restart required")

// Targets management endpoints (spec §5.2, D10): /hpc-config* is retired,
// scheduler templates belong to targets. GET /targets is the management
// view (full TargetConfig); GET /config stays the bootstrap summary. The
// GUI form is SCHEMA-DRIVEN: placeholder vocabulary ships from
// config.HPCPlaceholders — single source of truth (C3).

type targetsListResponse struct {
	Items        []config.TargetConfig `json:"items"`
	Placeholders map[string][]string   `json:"placeholders"`
	Path         string                `json:"path"`
	// ConfigGeneration is config.yaml's semantic content hash at read time
	// (RQ-75). The edit form stores it and sends it back as If-Match; a
	// mismatch on save means someone else changed the file in between.
	ConfigGeneration string `json:"config_generation"`
}

type targetCheckResponse struct {
	Results []config.HPCCheckResult `json:"results"`
}

type targetPresetsResponse struct {
	// Names preserves the canonical order (maps don't).
	Names   []string                       `json:"names"`
	Presets map[string]config.TargetConfig `json:"presets"`
}

// handleTargetPresets serves the same starter templates the CLI writes —
// one source, both personas (C3). "presets" is a reserved target name.
func (s *Server) handleTargetPresets(w http.ResponseWriter, r *http.Request) {
	resp := targetPresetsResponse{Names: config.HPCPresets(), Presets: map[string]config.TargetConfig{}}
	for _, name := range resp.Names {
		if cfg, err := config.HPCPreset(name); err == nil {
			resp.Presets[name] = *cfg
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleListTargets — GET /targets: management view, full TargetConfig
// fields (spec §4/§5.2). Local config file, no freshness semantics.
func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, targetsListResponse{
		Items:            cfg.ResolveTargets(),
		Placeholders:     config.HPCPlaceholders,
		Path:             config.ConfigPath(),
		ConfigGeneration: cfg.Generation,
	})
}

// handlePutTarget — PUT /targets/{name}: upsert one targets[] entry in
// config.yaml. The path name wins over any name in the body (D11:
// addressing is explicit).
func (s *Server) handlePutTarget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var tc config.TargetConfig
	if err := json.NewDecoder(r.Body).Decode(&tc); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	tc.Name = name

	cfg, err := config.Load()
	if err != nil {
		writeError(w, err)
		return
	}
	replaced := false
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == name {
			cfg.Targets[i] = tc
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Targets = append(cfg.Targets, tc)
	}
	if err := config.SaveGlobal(cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

// handleDeleteTarget — DELETE /targets/{name}.
func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := config.Load()
	if err != nil {
		writeError(w, err)
		return
	}
	kept := cfg.Targets[:0]
	found := false
	for _, t := range cfg.Targets {
		if t.Name == name {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		writeErr(w, http.StatusNotFound, backend.CodeNotFound, "target "+name+" not found")
		return
	}
	cfg.Targets = kept
	if err := config.SaveGlobal(cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

// handleCheckTarget validates the PROVIDED config without saving —
// preview is truth: the user sees exactly what check will say before
// commit. The {name} is addressing only; the body is what's checked.
func (s *Server) handleCheckTarget(w http.ResponseWriter, r *http.Request) {
	var tc config.TargetConfig
	if err := json.NewDecoder(r.Body).Decode(&tc); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, targetCheckResponse{Results: tc.CheckHPC()})
}

// SetForwardStarter installs the runtime forward hook (client daemon only).
func (s *Server) SetForwardStarter(fn func(name string) error) { s.forwardStarter = fn }

// SetForwardStatus wires the client daemon's forward-status snapshot into
// /health (RQ-74).
func (s *Server) SetForwardStatus(fn func() map[string]rfs.ForwardStatus) { s.forwardStatus = fn }

// SetForwardStopper wires the client daemon's runtime forward teardown
// (POST /targets/{name}/disconnect).
func (s *Server) SetForwardStopper(fn func(name string) error) { s.forwardStopper = fn }

// handleDisconnectTarget — POST /targets/{name}/disconnect: stop the remote
// CLI forward at runtime. Idempotent; config (remote_cli: false) is the
// CLI's job, this endpoint only handles the live daemon state.
func (s *Server) handleDisconnectTarget(w http.ResponseWriter, r *http.Request) {
	if s.forwardStopper == nil {
		notImplemented(w, "remote CLI forward")
		return
	}
	if err := s.forwardStopper(r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

// handleConnectTarget — POST /targets/{name}/connect: start or replace the
// target's remote CLI forward NOW, against the just-saved config. This is
// what lets `runq connect` take effect without a daemon restart. The one
// genuine restart case — the target has no lane because it was added after
// daemon start — comes back as 409 with the restart instruction.
func (s *Server) handleConnectTarget(w http.ResponseWriter, r *http.Request) {
	if s.forwardStarter == nil {
		notImplemented(w, "remote CLI forward")
		return
	}
	if err := s.forwardStarter(r.PathValue("name")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrForwardRestartRequired) {
			status = http.StatusConflict
		}
		writeErr(w, status, backend.CodeInvalidState, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

// handleRefreshTarget — POST /targets/{name}/refresh (spec §3 契约 3, D22):
// kick the lane's sensor for one forced full pass and wait for the pass
// that started AFTER the kick (generation fence in the lane), bounded by
// the request timeout. min_interval throttling and honest receipts are
// the lane's job; this handler only sets the wait budget.
func (s *Server) handleRefreshTarget(w http.ResponseWriter, r *http.Request) {
	fr, ok := s.backend.(interface {
		ForceRefreshTarget(context.Context, string) (*backend.RefreshReceipt, error)
	})
	if !ok {
		notImplemented(w, "target refresh")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	receipt, err := fr.ForceRefreshTarget(ctx, r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

type globalConfigPayload struct {
	DataPath      string `json:"data_path"`
	DefaultTarget string `json:"default_target"`
}

// handlePutGlobalConfig — PUT /config (D5: mode is gone). Writes global
// keys via the same SetKey path the CLI uses. Takes effect on next start
// — the GUI says so explicitly.
func (s *Server) handlePutGlobalConfig(w http.ResponseWriter, r *http.Request) {
	var p globalConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	if p.DataPath != "" {
		if err := config.SetKey("data_path", p.DataPath); err != nil {
			writeError(w, err)
			return
		}
	}
	if p.DefaultTarget != "" {
		if err := config.SetKey("default_target", p.DefaultTarget); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}
