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

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/utils"
)

// FSEntry, ParseScriptRequest, ParseResult, ScriptArg are in types.go.

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
	out := make([]FSEntry, 0, len(entries))
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
		out = append(out, FSEntry{
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
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleParseScript(w http.ResponseWriter, r *http.Request) {
	var req ParseScriptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	safePath, err := homeSafePath(req.Path)
	if err != nil {
		writeErrorStatus(w, http.StatusForbidden, err)
		return
	}
	args, err := job.ScanArgparse(safePath)
	if err != nil {
		writeErrorStatus(w, http.StatusBadRequest, err)
		return
	}
	out := ParseResult{
		Args: make([]ScriptArg, 0, len(args)),
		Env:  detectedEnv(filepath.Dir(safePath)),
		Cmd:  fmt.Sprintf("python %s {{args}}", filepath.Base(safePath)),
	}
	for _, arg := range args {
		var def *string
		if arg.Default != "" {
			value := arg.Default
			def = &value
		}
		out.Args = append(out.Args, ScriptArg{
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

func (s *Server) handleCondaEnvs(w http.ResponseWriter, r *http.Request) {
	// Try conda info --envs --json
	cmd := exec.Command("conda", "info", "--envs", "--json")
	out, err := cmd.Output()
	if err != nil {
		// conda not installed or not in PATH
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	var info struct {
		Envs []string `json:"envs"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		writeJSON(w, http.StatusOK, []string{})
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
	writeJSON(w, http.StatusOK, names)
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
