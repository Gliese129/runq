package project

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const yamlHeader = `# runq project configuration
# Edit as needed, then re-register: runq project add .
#
`

// WriteYAML writes the Config as a project.yaml file into the WorkingDir.
// If the file already exists, it is left untouched (user may have comments
// or custom formatting). Use OverwriteYAML to force-write.
// Returns an error if WorkingDir does not exist or is not a directory.
func (c *Config) WriteYAML() error {
	if err := c.checkWorkingDir(); err != nil {
		return err
	}
	path := filepath.Join(c.WorkingDir, "project.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil // already exists, don't clobber
	}
	return c.OverwriteYAML()
}

// OverwriteYAML writes the Config as project.yaml, replacing any existing file.
// Returns an error if WorkingDir does not exist or is not a directory.
func (c *Config) OverwriteYAML() error {
	if err := c.checkWorkingDir(); err != nil {
		return err
	}
	path := filepath.Join(c.WorkingDir, "project.yaml")
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	content := yamlHeader + string(data)
	return os.WriteFile(path, []byte(content), 0o644)
}

func (c *Config) checkWorkingDir() error {
	if c.WorkingDir == "" {
		return fmt.Errorf("working_dir is required")
	}
	info, err := os.Stat(c.WorkingDir)
	if err != nil {
		return fmt.Errorf("working_dir %q: %w", c.WorkingDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working_dir %q is not a directory", c.WorkingDir)
	}
	return nil
}

// Config represents a parsed project.yaml file.
type Config struct {
	ProjectName string            `yaml:"project_name" json:"project_name"`
	WorkingDir  string            `yaml:"working_dir" json:"working_dir"`
	CmdTemplate string            `yaml:"command_template" json:"command_template"`
	Environment map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
	// SetupCommand optionally runs ONCE per job submission, on the node where
	// runq runs (login node in HPC mode), after the plan compiles and before
	// anything is persisted or submitted — a failure aborts with zero residue.
	// Typical use: model pre-download. {{placeholder}}s may reference
	// fixed_params only (it runs once; swept params are ambiguous → error).
	// Runs synchronously: long setups are CLI-submission territory.
	SetupCommand string `yaml:"setup_command,omitempty" json:"setup_command,omitempty"`
	// EnvFile is an ambient env file sourced by the shell AT TASK START —
	// runq never parses or stores its values (secrets stay out of the DB,
	// run.sh exports, and every UI; rotation applies on the next task).
	// Precedence: .env < explicit env (environment / overrides.env).
	// nil → auto: use <working_dir>/.env when present. "" → disabled.
	// other → path (relative to working_dir), missing file = submit error.
	EnvFile *string `yaml:"env_file,omitempty" json:"env_file,omitempty"`
	Defaults    Defaults          `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Resume      ResumeConfig      `yaml:"resume,omitempty" json:"resume,omitempty"`
	PythonEnv   PythonEnvConfig   `yaml:"python_env,omitempty" json:"python_env,omitempty"`
	Wandb       *WandbConfig      `yaml:"wandb,omitempty" json:"wandb,omitempty"`
	Params      []ParamDef        `yaml:"params,omitempty" json:"params,omitempty"`
}

// ParamDef defines a parameter's metadata for the web UI.
// Discovered from script argparse or manually defined.
type ParamDef struct {
	Name    string   `yaml:"name" json:"name"`
	Type    string   `yaml:"type" json:"type"` // int, float, str, bool, file, folder, list
	Default string   `yaml:"default,omitempty" json:"default,omitempty"`
	Choices []string `yaml:"choices,omitempty" json:"choices,omitempty"` // selectable values for str/file/folder
	Min     *float64 `yaml:"min,omitempty" json:"min,omitempty"`         // valid range for int/float
	Max     *float64 `yaml:"max,omitempty" json:"max,omitempty"`         // valid range for int/float
	// Include is the user's curation: whether this param appears in the
	// submit flow's param table. Pointer on purpose — nil means "never
	// curated" (older configs / fresh discovery), which lets the UI apply
	// its first-time heuristic exactly once and persist the result.
	Include *bool `yaml:"include,omitempty" json:"include,omitempty"`
	// Strict upgrades Choices from suggestions to a contract: any submitted
	// value outside Choices is a submit-time error (enforced in
	// submitplan.Build — one point, both CLI and GUI).
	Strict bool `yaml:"strict,omitempty" json:"strict,omitempty"`
}

type WandbConfig struct {
	Project string   `yaml:"project" json:"project"`
	Entity  string   `yaml:"entity,omitempty" json:"entity,omitempty"`
	Tags    []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	Mode    string   `yaml:"mode,omitempty" json:"mode,omitempty"` // "online" / "offline" / "disabled"
}

// Defaults are project-level defaults that can be overridden per-job.
type Defaults struct {
	GPUsPerTask int    `yaml:"gpus_per_task,omitempty" json:"gpus_per_task,omitempty"`
	MaxRetry    int    `yaml:"max_retry,omitempty" json:"max_retry,omitempty"` // 0 means unlimited
	Timeout     string `yaml:"timeout,omitempty" json:"timeout,omitempty"`     // human duration, e.g. "3h", "1d"
}

// PythonEnvConfig specifies which Python environment to activate before running tasks.
type PythonEnvConfig struct {
	Type string `yaml:"type,omitempty" json:"type,omitempty"` // "venv", "conda", "uv", "system", "" (auto)
	Path string `yaml:"path,omitempty" json:"path,omitempty"` // venv: relative path to venv dir (e.g. ".venv")
	Name string `yaml:"name,omitempty" json:"name,omitempty"` // conda: environment name
}

// ResumeConfig controls whether a crashed task can resume from checkpoint.
type ResumeConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	ExtraArgs string `yaml:"extra_args,omitempty" json:"extra_args,omitempty"` // appended to cmd when resuming
}
