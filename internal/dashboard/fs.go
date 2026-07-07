package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/utils"
)

// fs group — /targets/{name}/fs/* (spec §5.2). Addressing is per-target;
// TODO(L3, #44): route through the target's rfs.FS (LocalFS/SSHFS) so
// remote targets browse THEIR filesystem. Until then the implementation
// is local-machine only (correct for local / login-node targets).

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = home
	}
	safePath, err := homeSafePath(path)
	if err != nil {
		writeErrorStatus(w, http.StatusForbidden, err)
		return
	}
	entries, err := os.ReadDir(safePath)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
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
		// unenterable. Stat the target instead. Broken links stay files.
		if !isDir && info.Mode()&os.ModeSymlink != 0 {
			if target, terr := os.Stat(filepath.Join(safePath, entry.Name())); terr == nil {
				isDir = target.IsDir()
			}
		}
		out = append(out, backend.FSEntry{
			Name:  entry.Name(),
			Path:  filepath.Join(safePath, entry.Name()),
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
	now := time.Now().Unix() // local read = fresh; real cache stamps in L4
	stale := false
	env.RefreshedAt, env.Stale = &now, &stale
	writeJSON(w, http.StatusOK, env)
}

// handleFSRead returns a text file's content (size-capped). Generic on
// purpose: the GUI imports project.yaml / job.yaml that live on THIS
// machine's filesystem (login node), not in the user's browser.
func (s *Server) handleFSRead(w http.ResponseWriter, r *http.Request) {
	safePath, err := homeSafePath(r.URL.Query().Get("path"))
	if err != nil {
		writeErrorStatus(w, http.StatusForbidden, err)
		return
	}
	info, err := os.Stat(safePath)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	const maxRead = 1 << 20 // 1 MiB — configs, not datasets
	if info.IsDir() || info.Size() > maxRead {
		writeErrorStatus(w, http.StatusBadRequest, fmt.Errorf("not a readable text file (dir or >1MiB)"))
		return
	}
	buf, err := os.ReadFile(safePath)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
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
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	safePath, err := homeSafePath(req.Path)
	if err != nil {
		writeErrorStatus(w, http.StatusForbidden, err)
		return
	}

	// Non-python entrypoints (.sh etc.): argparse scanning doesn't apply,
	// but env detection (conda/venv/uv markers in the directory) still does
	// — don't throw that away with a 400.
	if !strings.HasSuffix(safePath, ".py") {
		writeJSON(w, http.StatusOK, backend.ParseResult{
			Args: []backend.ScriptArg{},
			Env:  detectedEnv(filepath.Dir(safePath)),
			Cmd:  fmt.Sprintf("bash %s {{args}}", filepath.Base(safePath)),
		})
		return
	}

	args, err := job.ScanArgparse(safePath)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	out := backend.ParseResult{
		Args: make([]backend.ScriptArg, 0, len(args)),
		Env:  detectedEnv(filepath.Dir(safePath)),
		Cmd:  fmt.Sprintf("python %s {{args}}", filepath.Base(safePath)),
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

// homeSafePath confines browsing to $HOME — LEXICALLY, on purpose. The
// fence judges the path as typed/navigated, not the physical target, so a
// symlink under home (~/fast -> /scratch/...) is traversable. This is a
// guardrail against mistakes, not a security boundary: runq runs with the
// user's own permissions and the OS is the real boundary (philosophy C4);
// an attacker who can plant symlinks in your home has already won.
func homeSafePath(path string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "" {
		path = home
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	home, _ = filepath.Abs(home)
	abs = filepath.Clean(abs)
	home = filepath.Clean(home)
	if abs != home && !strings.HasPrefix(abs, home+string(os.PathSeparator)) {
		return "", fmt.Errorf("path outside home is not allowed")
	}
	return abs, nil
}

// handlePythonEnvs — GET /targets/{name}/python-envs (spec §5.2, D13:
// renamed from /conda/envs — the endpoint doesn't promise a conda
// implementation). Currently conda-backed and local-only; TODO(L3, #44):
// per-target via rfs Exec.
func (s *Server) handlePythonEnvs(w http.ResponseWriter, r *http.Request) {
	writeEnvs := func(names []string) {
		env := envelope(names)
		now := time.Now().Unix()
		stale := false
		env.RefreshedAt, env.Stale = &now, &stale
		writeJSON(w, http.StatusOK, env)
	}
	// Try conda info --envs --json
	cmd := exec.Command("conda", "info", "--envs", "--json")
	out, err := cmd.Output()
	if err != nil {
		// conda not installed or not in PATH
		writeEnvs(nil)
		return
	}
	var info struct {
		Envs []string `json:"envs"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		writeEnvs(nil)
		return
	}
	// Extract env names from paths: /path/to/envs/myenv → myenv, base is special
	names := make([]string, 0, len(info.Envs))
	seen := map[string]bool{}
	for _, p := range info.Envs {
		name := filepath.Base(p)
		// conda base env's path ends with the conda install dir, not "base"
		// Check if this is the base env by looking for conda-meta
		if _, err := os.Stat(filepath.Join(p, "conda-meta")); err != nil {
			continue
		}
		// The first env in the list is base
		if len(names) == 0 && name != "base" {
			// Check if it's the root conda dir
			if _, err := os.Stat(filepath.Join(p, "condabin")); err == nil {
				name = "base"
			}
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	writeEnvs(names)
}

func detectedEnv(dir string) string {
	envType, envPath, envName := utils.DetectPythonEnv(dir)
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
