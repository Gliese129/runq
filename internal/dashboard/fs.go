package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/utils"
)

// fs group — /targets/{name}/fs/* (spec §5.2, #44): the browser operates
// on the ADDRESSED TARGET's filesystem via rfs.FS (LocalFS/SSHFS — same
// code path, the handler doesn't know the difference).
//
// Protection is SOFT: path normalization only, no fence. The daemon runs
// with the user's own permissions and the OS is the real boundary
// (philosophy C4) — HPC data disks (/scratch, /data) outside home are a
// first-class use case, not an escape.

// targetFS resolves the addressed target's filesystem.
func (s *Server) targetFS(name string) (rfs.FS, error) {
	if tf, ok := s.backend.(interface {
		TargetFS(name string) (rfs.FS, error)
	}); ok {
		return tf.TargetFS(name)
	}
	return rfs.NewLocalFS(), nil
}

// isLocalFS reports whether the FS is the daemon's own disk — path
// normalization and env detection differ (Windows local paths; remote is
// always POSIX).
func isLocalFS(fsys rfs.FS) bool {
	_, ok := fsys.(*rfs.LocalFS)
	return ok
}

// cleanPath normalizes (soft protection: normalization is ALL it does).
// Empty path resolves to the FS's start dir (local home / remote home via
// the optional Home() interface).
func cleanPath(fsys rfs.FS, p string) string {
	if p == "" {
		return startDir(fsys)
	}
	if isLocalFS(fsys) {
		if abs, err := filepath.Abs(p); err == nil {
			return filepath.Clean(abs)
		}
		return filepath.Clean(p)
	}
	return path.Clean(p) // remote: POSIX rules, relative = sftp home-relative
}

// startDir is the browser's default entry point: the user's home on
// either side.
func startDir(fsys rfs.FS) string {
	if h, ok := fsys.(interface{ Home() (string, error) }); ok {
		if home, err := h.Home(); err == nil && home != "" {
			return home
		}
	}
	if isLocalFS(fsys) {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return "."
}

// joinPath joins with the FS's path rules.
func joinPath(fsys rfs.FS, dir, name string) string {
	if isLocalFS(fsys) {
		return filepath.Join(dir, name)
	}
	return path.Join(dir, name)
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	fsys, err := s.targetFS(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	dir := cleanPath(fsys, r.URL.Query().Get("path"))

	entries, err := fsys.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	out := make([]backend.FSEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		isDir := entry.IsDir()
		// Symlinks: DirEntry reports the LINK's type, so a symlinked
		// directory (~/fast -> /scratch/...) would show as a file and become
		// unenterable. Stat the target instead (rfs.Stat follows links on
		// both sides). Broken links stay files.
		if !isDir && info.Mode()&os.ModeSymlink != 0 {
			if target, terr := fsys.Stat(joinPath(fsys, dir, entry.Name())); terr == nil {
				isDir = target.IsDir()
			}
		}
		out = append(out, backend.FSEntry{
			Name:  entry.Name(),
			Path:  joinPath(fsys, dir, entry.Name()),
			IsDir: isDir,
			Size:  info.Size(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	env := envelope(out)
	now := time.Now().Unix() // TODO(L4): light-cache stamps
	stale := false
	env.RefreshedAt, env.Stale = &now, &stale
	writeJSON(w, http.StatusOK, env)
}

// handleFSRead returns a text file's content (size-capped). Generic on
// purpose: the GUI imports project.yaml / job.yaml that live on the
// TARGET's filesystem, wherever that is.
func (s *Server) handleFSRead(w http.ResponseWriter, r *http.Request) {
	fsys, err := s.targetFS(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	p := cleanPath(fsys, r.URL.Query().Get("path"))

	info, err := fsys.Stat(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	const maxRead = 1 << 20 // 1 MiB — configs, not datasets
	if info.IsDir() || info.Size() > maxRead {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, "not a readable text file (dir or >1MiB)")
		return
	}
	buf, err := fsys.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content":      string(buf),
		"refreshed_at": time.Now().Unix(),
	})
}

func (s *Server) handleParseScript(w http.ResponseWriter, r *http.Request) {
	var req backend.ParseScriptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	fsys, err := s.targetFS(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	p := cleanPath(fsys, req.Path)
	base := entryBase(fsys, p)

	// Env detection probes marker files through the target's FS — remote
	// scripts get suggestions too (a handful of small sftp reads).
	env := detectedEnv(fsys, dirOf(fsys, p))

	// Non-python entrypoints (.sh etc.): argparse scanning doesn't apply,
	// but the command scaffold is still useful — don't throw that away
	// with a 400.
	if !strings.HasSuffix(p, ".py") {
		writeJSON(w, http.StatusOK, backend.ParseResult{
			Args: []backend.ScriptArg{},
			Env:  env,
			Cmd:  fmt.Sprintf("bash %s {{args}}", base),
		})
		return
	}

	content, err := fsys.ReadFile(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	args, err := job.ScanArgparseBytes(content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, backend.CodeBadRequest, err.Error())
		return
	}
	out := backend.ParseResult{
		Args: make([]backend.ScriptArg, 0, len(args)),
		Env:  env,
		Cmd:  fmt.Sprintf("python %s {{args}}", base),
	}
	for _, arg := range args {
		var def *string
		if arg.Default != "" {
			value := arg.Default
			def = &value
		}
		out.Args = append(out.Args, backend.ScriptArg{
			Name:    arg.Name,
			Type:    arg.Type,
			Default: def,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func entryBase(fsys rfs.FS, p string) string {
	if isLocalFS(fsys) {
		return filepath.Base(p)
	}
	return path.Base(p)
}

func dirOf(fsys rfs.FS, p string) string {
	if isLocalFS(fsys) {
		return filepath.Dir(p)
	}
	return path.Dir(p)
}

// handlePythonEnvs — GET /targets/{name}/python-envs (spec §5.2, D13:
// renamed from /conda/envs — the endpoint doesn't promise a conda
// implementation). Runs `conda info` ON THE TARGET via FS.Exec, so remote
// targets report their own environments.
func (s *Server) handlePythonEnvs(w http.ResponseWriter, r *http.Request) {
	writeEnvs := func(names []string) {
		env := envelope(names)
		now := time.Now().Unix()
		stale := false
		env.RefreshedAt, env.Stale = &now, &stale
		writeJSON(w, http.StatusOK, env)
	}
	fsys, err := s.targetFS(r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	stdout, _, code, err := fsys.Exec(ctx, "conda", "info", "--envs", "--json")
	if err != nil || code != 0 {
		// conda not installed / not in PATH — no envs to offer.
		writeEnvs(nil)
		return
	}
	var info struct {
		Envs []string `json:"envs"`
	}
	if err := json.Unmarshal(stdout, &info); err != nil {
		writeEnvs(nil)
		return
	}
	// Extract env names from paths: .../envs/myenv → myenv. conda lists the
	// base (install root) first; its basename isn't "base", so map it.
	names := make([]string, 0, len(info.Envs))
	seen := map[string]bool{}
	for i, p := range info.Envs {
		name := path.Base(p)
		if i == 0 && name != "base" {
			name = "base"
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	writeEnvs(names)
}

// detectedEnv formats DetectPythonEnv's triple as the GUI's "type:name"
// suggestion string. Pure formatting — FS handling lives in utils.
func detectedEnv(fsys rfs.FS, dir string) string {
	envType, envPath, envName := utils.DetectPythonEnv(fsys, dir)
	if envType == "" {
		return ""
	}
	if envName != "" {
		return envType + ":" + envName
	}
	if envPath != "" {
		return envType + ":" + envPath
	}
	return envType
}
