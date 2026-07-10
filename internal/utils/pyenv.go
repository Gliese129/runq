package utils

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gliese129/runq/internal/rfs"
)

// WrapCommand prepends shell commands to activate the given Python environment
// before running the actual task command. Returns the command unchanged
// if envType is "system", empty, or unrecognized.
//
// envType: "venv", "uv", "conda", "system", or "" (no-op).
// envPath: venv/uv directory (e.g. ".venv"); relative paths resolved against workingDir.
// envName: conda environment name (e.g. "torch").
func WrapCommand(envType, envPath, envName, cmd, workingDir string) string {
	switch envType {
	case "venv", "uv":
		// uv creates standard venvs — activation is identical.
		path := envPath
		if path == "" {
			path = ".venv"
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workingDir, path)
		}
		return ". " + shellQuote(filepath.Join(path, "bin", "activate")) + " && " + cmd
	case "conda":
		name := envName
		if name == "" {
			name = "base"
		}
		// conda activate requires shell initialization first.
		return `eval "$(conda shell.bash hook)" && conda activate ` + shellQuote(name) + " && " + cmd
	default:
		// "system", "", or unknown — run as-is.
		return cmd
	}
}

// DetectPythonEnv auto-detects the Python environment for a project
// directory, through the given filesystem — the directory may live on a
// remote target (marker probes are a handful of small sftp reads).
// fsys == nil defaults to the local filesystem (codebase convention).
// Returns (envType, envPath, envName):
//   - envType: "uv", "venv", "conda", or "" (system/unknown)
//   - envPath: relative venv directory (e.g. ".venv"); empty for conda/system
//   - envName: conda environment name; empty for venv/system
//
// Detection order: uv (.venv/pyvenv.cfg contains "uv") → venv
// (.venv/bin/activate) → conda ($CONDA_DEFAULT_ENV, LOCAL ONLY — an env
// var describes the daemon's own shell, meaningless for a remote dir) →
// uv (pyproject.toml [tool.uv]).
func DetectPythonEnv(fsys rfs.FS, workingDir string) (string, string, string) {
	local := false
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	if _, ok := fsys.(*rfs.LocalFS); ok {
		local = true
	}
	// Remote dirs are POSIX; local may be Windows.
	join := path.Join
	if local {
		join = filepath.Join
	}

	uvPath := join(workingDir, ".venv", "pyvenv.cfg")
	if data, err := fsys.ReadFile(uvPath); err == nil {
		if strings.Contains(string(data), "uv") {
			return "uv", ".venv", ""
		}
	}
	// Standard venv (no uv marker).
	venvPath := join(workingDir, ".venv", "bin", "activate")
	if _, err := fsys.Stat(venvPath); err == nil {
		return "venv", ".venv", ""
	}
	// Conda: $CONDA_DEFAULT_ENV is the daemon process's own environment —
	// only meaningful when the directory is on the same machine.
	if local {
		if condaEnv := os.Getenv("CONDA_DEFAULT_ENV"); condaEnv != "" {
			return "conda", "", condaEnv
		}
	}
	// Fallback: pyproject.toml with [tool.uv] section (no .venv created yet).
	// Returns ".venv" as conventional path — WrapCommand will try to activate it.
	// If user hasn't run `uv sync` yet, activation will fail at runtime.
	// TODO: add poetry support when WrapCommand learns poetry shell activation.
	tomlPath := join(workingDir, "pyproject.toml")
	if content, err := fsys.ReadFile(tomlPath); err == nil {
		if strings.Contains(string(content), "[tool.uv]") {
			return "uv", ".venv", ""
		}
	}
	return "", "", ""
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
