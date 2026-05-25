package project

// Config represents a parsed project.yaml file.
type Config struct {
	ProjectName string            `yaml:"project_name"`
	WorkingDir  string            `yaml:"working_dir"`
	CmdTemplate string            `yaml:"command_template"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Defaults    Defaults          `yaml:"defaults,omitempty"`
	Resume      ResumeConfig      `yaml:"resume,omitempty"`
	PythonEnv   PythonEnvConfig   `yaml:"python_env,omitempty"`
	Wandb       *WandbConfig      `yaml:"wandb,omitempty"`
}

type WandbConfig struct {
	Project string   `yaml:"project"`
	Entity  string   `yaml:"entity,omitempty"`
	Tags    []string `yaml:"tags,omitempty"`
	Mode    string   `yaml:"mode,omitempty"` // "online" / "offline" / "disabled"
}

// Defaults are project-level defaults that can be overridden per-job.
type Defaults struct {
	GPUsPerTask int    `yaml:"gpus_per_task,omitempty"`
	MaxRetry    int    `yaml:"max_retry,omitempty"` // 0 means unlimited
	Timeout     string `yaml:"timeout,omitempty"`   // human duration, e.g. "3h", "1d"
}

// PythonEnvConfig specifies which Python environment to activate before running tasks.
type PythonEnvConfig struct {
	Type string `yaml:"type,omitempty"` // "venv", "conda", "uv", "system", "" (auto)
	Path string `yaml:"path,omitempty"` // venv: relative path to venv dir (e.g. ".venv")
	Name string `yaml:"name,omitempty"` // conda: environment name
}

// ResumeConfig controls whether a crashed task can resume from checkpoint.
type ResumeConfig struct {
	Enabled   bool   `yaml:"enabled"`
	ExtraArgs string `yaml:"extra_args,omitempty"` // appended to cmd when resuming
}
