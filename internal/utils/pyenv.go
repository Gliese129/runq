package utils

import (
	"os"
	"path/filepath"
	"strings"
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
		return "source " + path + "/bin/activate && " + cmd
	case "conda":
		name := envName
		if name == "" {
			name = "base"
		}
		// conda activate requires shell initialization first.
		return `eval "$(conda shell.bash hook)" && conda activate ` + name + " && " + cmd
	default:
		// "system", "", or unknown — run as-is.
		return cmd
	}
}

func DetectPythonEnv(workingDir string) (string, string, string) {
	// uv first
	uvPath := filepath.Join(workingDir, ".venv", "pyvenv.cfg")
	if data, err := os.ReadFile(uvPath); err == nil {
		if strings.Contains(string(data), "uv") {
			return "uv", ".venv", ""
		}
	}
	// then venv
	venvPath := filepath.Join(workingDir, ".venv", "bin", "activate")
	_, err := os.Stat(venvPath)
	if err == nil {
		return "venv", ".venv", ""
	}
	// conda from env
	if condaEnv := os.Getenv("CONDA_DEFAULT_ENV"); condaEnv != "" {
		return "conda", "", condaEnv
	}
	// poetry/uv from tomlPath (this is a little bit complex)
	// poetry not support yet
	tomlPath := filepath.Join(workingDir, "pyproject.toml")
	if content, err := os.ReadFile(tomlPath); err == nil {
		strContent := string(content)
		//if strings.Contains(strContent, "[tool.poetry]") {
		//	return "poetry", "", ""
		//}
		if strings.Contains(strContent, "[tool.uv]") {
			return "uv", "", ""
		}
	}
	return "", "", ""
}
