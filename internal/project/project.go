package project

// Config represents a parsed project.yaml file.
type Config struct {
	ProjectName string            `yaml:"project_name"`
	WorkingDir  string            `yaml:"working_dir"`
	CmdTemplate string            `yaml:"command_template"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Defaults    Defaults          `yaml:"defaults,omitempty"`
	Resume      ResumeConfig      `yaml:"resume,omitempty"`
}

// Defaults are project-level defaults that can be overridden per-job.
type Defaults struct {
	GPUsPerTask int `yaml:"gpus_per_task,omitempty"`
	MaxRetry    int `yaml:"max_retry,omitempty"` // 0 means unlimited
}

// ResumeConfig controls whether a crashed task can resume from checkpoint.
type ResumeConfig struct {
	Enabled   bool   `yaml:"enabled"`
	ExtraArgs string `yaml:"extra_args,omitempty"` // appended to cmd when resuming
}
