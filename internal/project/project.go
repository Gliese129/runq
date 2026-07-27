package project

import (
	"fmt"
	"path"
	"regexp"

	"github.com/gliese129/runq/internal/rfs"

	"gopkg.in/yaml.v3"
)

const yamlHeader = `# runq project configuration
# Edit as needed, then re-register: runq project add .
#
`

// WriteYAML writes the Config as a project.yaml file into the WorkingDir
// ON THE PROJECT'S OWN FILESYSTEM (fsys; nil = local). If the file already
// exists there, it is left untouched (user may have comments or custom
// formatting). Use OverwriteYAML to force-write.
//
// No pre-flight stat of WorkingDir (RQ-65): registration is registration,
// not a health check — and the only machine entitled to answer "does this
// dir exist" is the project's own target. A write to a missing dir fails
// loudly with the target's error, which is the honest signal.
func (c *Config) WriteYAML(fsys rfs.FS) error {
	if c.WorkingDir == "" {
		return fmt.Errorf("working_dir is required")
	}
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	yamlPath := path.Join(c.WorkingDir, "project.yaml")
	if _, err := fsys.Stat(yamlPath); err == nil {
		return nil // already exists, don't clobber
	}
	return c.OverwriteYAML(fsys)
}

// OverwriteYAML writes the Config as project.yaml on the project's own
// filesystem, replacing any existing file.
func (c *Config) OverwriteYAML(fsys rfs.FS) error {
	if c.WorkingDir == "" {
		return fmt.Errorf("working_dir is required")
	}
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	content := yamlHeader + string(data)
	// Scaffold the preflight contract when the config has none: the block
	// spells out the defaults WITH comments, so the contract is
	// discoverable in the one file users actually read. Semantics are
	// identical with or without the block — this only makes them visible.
	if c.Preflight == nil {
		content += c.preflightScaffold()
	}
	return fsys.WriteFile(path.Join(c.WorkingDir, "project.yaml"), []byte(content), 0o644)
}

// hfParamNameRegex spots params whose values look like they carry HF
// repo ids — used only to pre-fill the scaffold suggestion.
var hfParamNameRegex = regexp.MustCompile(`(?i)^(hf_|model|.*model_name|.*model$|dataset|.*dataset$)`)

// preflightScaffold renders the commented preflight block appended to a
// freshly written project.yaml. Params with model/dataset-ish names are
// suggested as "{{param.X}}" entries (quoted — a bare {{ is not valid
// YAML); the user removes wrong guesses rather than authoring from zero.
func (c *Config) preflightScaffold() string {
	var hfLines string
	for _, p := range c.Params {
		if hfParamNameRegex.MatchString(p.Name) {
			hfLines += fmt.Sprintf("    - \"{{param.%s}}\"  # suggested from param names — remove if this is not a HuggingFace repo id\n", p.Name)
		}
	}
	hfBlock := "  hf: []              # HuggingFace repos to verify; \"{{param.NAME}}\" checks every sweep value\n"
	if hfLines != "" {
		hfBlock = "  hf:                 # HuggingFace repos to verify; \"{{param.NAME}}\" checks every sweep value\n" + hfLines
	}
	return `
# Submit-time checks (preflight). Declared entries are a contract: they are
# verified inside the task's env in a NON-INTERACTIVE shell (the shell class
# compute nodes use — no ~/.bashrc) on every submit; failures block before
# anything is queued. Undeclared checks fall back to best-effort static
# analysis of the entry .py.
preflight:
  enabled: true       # master switch; runq submit --no-preflight still skips per-run
  imports: true       # probe top-level imports of the entry .py (best effort)
  wandb: false        # verify wandb credentials when wandb is configured
` + hfBlock + `  extra_run: ""       # custom shell check; non-zero exit blocks submission
`
}

// Config represents a parsed project.yaml file.
type Config struct {
	ProjectName string `yaml:"project_name" json:"project_name"`
	// Target is the project's home: the compute target whose filesystem
	// owns WorkingDir and every other path in this config (RQ-65). All
	// path semantics — yaml sync, preflight, workspace layout — resolve
	// on that machine, never on whichever machine happens to run the
	// daemon. Empty = the local machine.
	Target      string            `yaml:"target,omitempty" json:"target,omitempty"`
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
	// JobName is a template for the per-task scheduler job name, exposed to
	// submit_template as {{name}}. Vocabulary: params + {{project}}
	// {{job_id}} {{task_id}}. job.yaml `name:` overrides per submission.
	// Always sanitized (scheduler-safe charset, never digit-first).
	// Empty → "rq-{{task_id}}".
	JobName string `yaml:"job_name,omitempty" json:"job_name,omitempty"`
	// EnvFile is an ambient env file sourced by the shell AT TASK START —
	// runq never parses or stores its values (secrets stay out of the DB,
	// run.sh exports, and every UI; rotation applies on the next task).
	// Precedence: .env < explicit env (environment / overrides.env).
	// nil → auto: use <working_dir>/.env when present. "" → disabled.
	// other → path (relative to working_dir), missing file = submit error.
	EnvFile   *string         `yaml:"env_file,omitempty" json:"env_file,omitempty"`
	Defaults  Defaults        `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Resume    ResumeConfig    `yaml:"resume,omitempty" json:"resume,omitempty"`
	PythonEnv PythonEnvConfig `yaml:"python_env,omitempty" json:"python_env,omitempty"`
	Wandb     *WandbConfig    `yaml:"wandb,omitempty" json:"wandb,omitempty"`
	Params    []ParamDef      `yaml:"params,omitempty" json:"params,omitempty"`
	// Preflight is the project's submit-time check CONTRACT (RQ-76 ②).
	// nil = the same defaults the scaffold makes visible — an absent block
	// never means a second set of semantics.
	Preflight *PreflightConfig `yaml:"preflight,omitempty" json:"preflight,omitempty"`
}

// PreflightConfig declares what preflight verifies for this project.
// Principle: what is DECLARED is a contract (verified exactly, failures
// block); what is not declared is best-effort static analysis. The
// checks run inside the task's env in a NON-INTERACTIVE shell — the
// shell class compute nodes give the task — which is what a manual
// login-shell check can never prove.
type PreflightConfig struct {
	// Enabled is the project-level master switch. nil/true = on.
	// `runq submit --no-preflight` still skips per-run.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Imports probes top-level imports of the entry .py (best effort;
	// shell entries have nothing to statically analyze). nil/true = on.
	Imports *bool `yaml:"imports,omitempty" json:"imports,omitempty"`
	// Wandb verifies wandb credentials (WANDB_API_KEY / ~/.netrc) are
	// visible where the task runs. nil/false = off.
	Wandb *bool `yaml:"wandb,omitempty" json:"wandb,omitempty"`
	// HF lists HuggingFace repos to verify. "{{param.NAME}}" entries
	// expand against the sweep — EVERY value is checked. Declared repos
	// that are missing or gated block the submit.
	HF []string `yaml:"hf,omitempty" json:"hf,omitempty"`
	// ExtraRun is a custom shell check executed inside the activated env
	// in the same probe shell; non-zero exit blocks the submit. The
	// catch-all for anything runq cannot see statically.
	ExtraRun string `yaml:"extra_run,omitempty" json:"extra_run,omitempty"`
}

// EnabledOrDefault: absent block / absent key = enabled.
func (p *PreflightConfig) EnabledOrDefault() bool {
	return p == nil || p.Enabled == nil || *p.Enabled
}

// ImportsOrDefault: absent = on (best-effort static analysis).
func (p *PreflightConfig) ImportsOrDefault() bool {
	return p == nil || p.Imports == nil || *p.Imports
}

// WandbOrDefault: absent = off (many projects run wandb-less).
func (p *PreflightConfig) WandbOrDefault() bool {
	return p != nil && p.Wandb != nil && *p.Wandb
}

// HFRepos returns the declared repo list (nil-safe).
func (p *PreflightConfig) HFRepos() []string {
	if p == nil {
		return nil
	}
	return p.HF
}

// ExtraRunOrEmpty returns the custom check command (nil-safe).
func (p *PreflightConfig) ExtraRunOrEmpty() string {
	if p == nil {
		return ""
	}
	return p.ExtraRun
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
	// Scope declares who consumes this param: "" / "command" (default —
	// the workload command) or "scheduler" (the HPC submit_template via
	// {{param.*}}). Scheduler params are invisible to the command renderer
	// (no unconsumed error, never injected into {{args}}) — declared here
	// so job.yaml + project.yaml stay self-describing across machines.
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
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

// UnlimitedRetries is the ONLY negative max_retry the contract admits.
// Intake boundaries (project save, submit planning) reject anything below
// it; the RUNTIME retry check stays tolerant (`< 0` = unlimited) so rows
// persisted before this rule existed cannot brick the scheduler.
const UnlimitedRetries = -1

// ValidateRetryBounds enforces the max_retry contract at intake.
func ValidateRetryBounds(maxRetry int) error {
	if maxRetry < UnlimitedRetries {
		return fmt.Errorf("max_retry %d out of range: use -1 (unlimited) or >= 0", maxRetry)
	}
	return nil
}

// Defaults are project-level defaults that can be overridden per-job.
//
// The int fields carry MEANINGFUL zeros (gpus_per_task 0 = CPU-only,
// max_retry 0 = no retries), and the scheduler already treats an unset
// field as 0 — so the JSON tags must NOT use omitempty: it silently
// dropped a legal explicit 0 from the wire and the dashboard rendered its
// form default (1 GPU) instead, then wrote that back (RQ-69 review).
// YAML keeps omitempty for hand-written project.yaml friendliness; an
// omitted key decodes to the same 0 the scheduler uses, so the file and
// wire semantics agree.
type Defaults struct {
	GPUsPerTask int    `yaml:"gpus_per_task,omitempty" json:"gpus_per_task"`
	MaxRetry    int    `yaml:"max_retry,omitempty" json:"max_retry"`       // -1 = unlimited, 0 = no retries
	Timeout     string `yaml:"timeout,omitempty" json:"timeout,omitempty"` // human duration, e.g. "3h", "1d"
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
