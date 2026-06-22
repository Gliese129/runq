package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/job"
)

// Config editing endpoints. The GUI form is SCHEMA-DRIVEN: every field is
// always rendered (registered) whether or not it exists in the file, and the
// placeholder vocabulary ships from hpcconfig.Placeholders — single source
// of truth, completion can never drift from the backend contract (C3).
// The CLI persona gets none of this hand-holding: `runq hpc config edit`.

type hpcConfigResponse struct {
	Exists       bool                `json:"exists"` // hpc: section present in file
	Config       hpcconfig.Config    `json:"config"`
	Placeholders map[string][]string `json:"placeholders"`
	Path         string              `json:"path"`
}

type hpcCheckResponse struct {
	Results []hpcconfig.CheckResult `json:"results"`
}

type hpcPresetsResponse struct {
	// Names preserves the canonical order (maps don't).
	Names   []string                    `json:"names"`
	Presets map[string]hpcconfig.Config `json:"presets"`
}

// handleHPCPresets serves the same starter templates `hpc init --scheduler`
// writes — one source, both personas (C3).
func (s *Server) handleHPCPresets(w http.ResponseWriter, r *http.Request) {
	resp := hpcPresetsResponse{Names: hpcconfig.Presets(), Presets: map[string]hpcconfig.Config{}}
	for _, name := range resp.Names {
		if cfg, err := hpcconfig.Preset(name); err == nil {
			resp.Presets[name] = *cfg
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetHPCConfig(w http.ResponseWriter, r *http.Request) {
	resp := hpcConfigResponse{
		Placeholders: hpcconfig.Placeholders,
		Path:         config.ConfigPath(),
	}
	if cfg, err := hpcconfig.Load(); err == nil {
		resp.Exists = true
		resp.Config = *cfg
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCheckHPCConfig validates the PROVIDED config without saving —
// preview is truth: the user sees exactly what check will say before commit.
func (s *Server) handleCheckHPCConfig(w http.ResponseWriter, r *http.Request) {
	var cfg hpcconfig.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, hpcCheckResponse{Results: cfg.Check()})
}

func (s *Server) handlePutHPCConfig(w http.ResponseWriter, r *http.Request) {
	var cfg hpcconfig.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	if err := hpcconfig.Save(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}

// handleResolveNote previews the job note's resolution via the backend
// (the {{version}} family scan needs the store) — submit's exact code path.
func (s *Server) handleResolveNote(w http.ResponseWriter, r *http.Request) {
	var cfg job.JobConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	resolved, err := s.backend.ResolveNote(r.Context(), cfg)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"resolved": resolved})
}

type globalConfigPayload struct {
	Mode     string `json:"mode"`
	DataPath string `json:"data_path"`
}

// handlePutGlobalConfig writes global keys via the same SetKey path the CLI
// uses. Takes effect on next start — the GUI says so explicitly.
func (s *Server) handlePutGlobalConfig(w http.ResponseWriter, r *http.Request) {
	var p globalConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	if p.Mode != "" {
		if err := config.SetKey("mode", p.Mode); err != nil {
			writeErrorStatus(w, http.StatusBadRequest, err)
			return
		}
	}
	if err := config.SetKey("data_path", p.DataPath); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}
