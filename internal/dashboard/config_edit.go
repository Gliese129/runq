package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

// Targets management endpoints (spec §5.2, D10): /hpc-config* is retired,
// scheduler templates belong to targets. GET /targets is the management
// view (full TargetConfig); GET /config stays the bootstrap summary. The
// GUI form is SCHEMA-DRIVEN: placeholder vocabulary ships from
// config.HPCPlaceholders — single source of truth (C3).

type targetsListResponse struct {
	Items        []config.TargetConfig `json:"items"`
	Placeholders map[string][]string   `json:"placeholders"`
	Path         string                `json:"path"`
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
		Items:        cfg.ResolveTargets(),
		Placeholders: config.HPCPlaceholders,
		Path:         config.ConfigPath(),
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

// handleRefreshTarget — POST /targets/{name}/refresh (spec §3 契约 3):
// force-refresh every cache for the target, guarded by the 5min floor.
// Returns the refresh receipt so the caller knows whether it actually
// refreshed (D22). Needs the cache layer (L4).
func (s *Server) handleRefreshTarget(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "target refresh") // TODO(L4): {refreshed_at, refreshed, reason?}
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
