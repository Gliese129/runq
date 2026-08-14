package dashboard

// GET/PUT /api/v1/ui — per-user UI state blob (RQ2-1 §D: project grouping
// + appearance preferences that should roam across machines with the
// daemon). The CONTENT schema belongs to the frontend; the backend
// persists an opaque JSON object at ConfigDir()/ui.json and never
// interprets it. Deliberately minimal contract:
//
//   - whole-document replace, no CAS — single-user file, and a lost
//     preference write costs nothing (unlike config.yaml's If-Match)
//   - 64KB cap: this is preferences, not data; the cap keeps a buggy
//     frontend from turning the file into a dumping ground
//   - tmp + rename write so a crash never leaves a torn document
//   - NOT in the Backend interface, NOT proxied, CLI never touches it —
//     UI state belongs to the machine serving the UI
//
// GET with no file (or an unreadable/corrupt one) returns {} — the
// frontend treats absence and emptiness identically, and localStorage
// remains its offline fallback.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
)

const maxUIStateBytes = 64 * 1024

func uiStatePath() string { return filepath.Join(config.ConfigDir(), "ui.json") }

func (s *Server) handleGetUIState(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(uiStatePath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warnf("ui state: read %s failed (serving empty state): %v", uiStatePath(), err)
		}
		data = []byte("{}")
	} else if !json.Valid(data) {
		// A torn/hand-mangled file is preferences lost, not an outage:
		// serve {} so the UI boots clean; the next PUT heals the file.
		logger.Warnf("ui state: %s is not valid JSON (serving empty state)", uiStatePath())
		data = []byte("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handlePutUIState(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUIStateBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > maxUIStateBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, backend.CodeBadRequest,
			"ui state exceeds the 64KB limit")
		return
	}
	trimmed := bytes.TrimSpace(body)
	// Opaque, but it must be a JSON OBJECT: GET's empty default is {} and
	// the frontend merges over it — an array or scalar would break every
	// consumer, so reject it here rather than persist a poison document.
	if !json.Valid(trimmed) || len(trimmed) == 0 || trimmed[0] != '{' {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "body must be a JSON object")
		return
	}

	dir := config.ConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".ui.json.*")
	if err != nil {
		writeError(w, err)
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(trimmed)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		writeError(w, errors.Join(werr, cerr))
		return
	}
	if err := os.Rename(tmpName, uiStatePath()); err != nil {
		_ = os.Remove(tmpName)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backend.ActionResponse{OK: true})
}
