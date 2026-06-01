// Package hpcconfig owns the HPC deployment's on-disk configuration and the
// resolution of its data directory. It is deliberately separate from the daemon
// data dir (utils.ResolveDataDir): HPC and daemon are separate deployments and
// must never share a runtime, DB, or control plane.
//
// Layout under the data dir (default ~/.runq):
//
//	~/.runq/
//	├── config.yaml          cluster templates (this package)
//	├── runq.db              HPC store (reuses internal/store schema)
//	└── <job_id>/<task_id>/  per-task workspace (params.json, run.sh, status.json, ...)
package hpcconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the parsed ~/.runq/config.yaml. The cluster dialect lives entirely
// in these templates — runq does no Slurm/PBS/SGE parsing, only interpolation
// (via hpccore.Render) and one user-supplied regex.
type Config struct {
	// SubmitTemplate is the shell command that queues a job. Available vars:
	// {{run_sh}} {{gpus}} {{job_id}} {{task_id}} {{task_dir}}.
	SubmitTemplate string `yaml:"submit_template"`
	// SubmitIDRegex extracts the external job id from the submit command output.
	// Must contain exactly one capture group.
	SubmitIDRegex string `yaml:"submit_id_regex"`
	// StatusTemplate optionally probes scheduler state. Var: {{ext_id}}.
	// Empty → status comes from status.json alone.
	//
	// WITHOUT a status_parser: the raw output is interpreted by hpccore.ParseSignal
	// (canonical tokens + Slurm sacct states); empty output = gone, present-but-
	// unrecognized = alive, and a non-zero command exit = no info (SchedUnknown).
	//
	// WITH a status_parser: this command's exit code is IGNORED — its output
	// (even empty, even on a non-zero exit) is handed to the parser. Deliberate:
	// active queries like `qstat -f <id>` ERROR once the job leaves the queue, and
	// that "error + empty output" is exactly the gone signal the parser turns into
	// `gone`.
	StatusTemplate string `yaml:"status_template,omitempty"`
	// StatusParser is an OPTIONAL pipeline that translates the scheduler's raw
	// output into a normalized token, for schedulers whose output isn't directly
	// recognizable (PBS qstat codes, SGE, etc.). It is a LIST of shell stages:
	// the raw status_template output is fed to stage 1 on stdin, each stage pipes
	// into the next, and the final stdout must be one of:
	//   pending | running | success | failed | killed | gone
	// runq assembles the pipe for you, so each stage stays a short, readable
	// filter instead of one giant one-liner. {{ext_id}} is available in any stage.
	//
	// CONTRACT: the PIPELINE (not the status_template) should EXIT 0 and PRINT a
	// token; print `gone` for an absent job. Empty pipeline output is read as
	// `gone`; a non-zero PIPELINE exit is "no info" (SchedUnknown, status
	// unchanged). Don't rely on `grep`'s exit code to mean gone — use an awk END
	// block to always emit a token. For reliable terminal states
	// (failed/timeout/oom), prefer an accounting query that always has the record
	// (the slurm preset uses `sacct`); active-only queries like squeue/qstat can't
	// always tell "gone" from "finished".
	//
	// runq ships no built-in dialect parser — this pipeline is the extension point.
	StatusParser []string `yaml:"status_parser,omitempty"`
	// KillTemplate cancels a queued/running job. Var: {{ext_id}}.
	KillTemplate string `yaml:"kill_template"`
}

// DataDir resolves the HPC data directory: RUNQ_DATA_DIR override, else ~/.runq.
// Unlike the daemon there is no root/privileged path — HPC is always per-user on
// a shared filesystem.
func DataDir() string {
	if dir := os.Getenv("RUNQ_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".runq")
}

// ConfigPath is <DataDir>/config.yaml.
func ConfigPath() string { return filepath.Join(DataDir(), "config.yaml") }

// DBPath is <DataDir>/runq.db (the HPC store, separate from the daemon DB).
func DBPath() string { return filepath.Join(DataDir(), "runq.db") }

// JobDir is the per-job workspace root <DataDir>/<job_id>; task dirs are
// <DataDir>/<job_id>/<task_id> (computed via workspace.TaskDir).
func JobDir(jobID string) string { return filepath.Join(DataDir(), jobID) }

// Load reads and validates config.yaml. A missing file is a clear, actionable
// error pointing the user at `runq hpc init`.
func Load() (*Config, error) {
	path := ConfigPath()
	buf, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no HPC config at %s — run `runq hpc init` first", path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(buf, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.SubmitTemplate == "" {
		return fmt.Errorf("submit_template is required")
	}
	if c.SubmitIDRegex == "" {
		return fmt.Errorf("submit_id_regex is required")
	}
	// Validate the regex at load so a typo is a clear config error here, not a
	// panic deep inside ExtractSubmitID (regexp.MustCompile) at submit time.
	re, err := regexp.Compile(c.SubmitIDRegex)
	if err != nil {
		return fmt.Errorf("submit_id_regex is not a valid regular expression: %w", err)
	}
	if re.NumSubexp() != 1 {
		return fmt.Errorf(
			`submit_id_regex must contain exactly one capture group for the job id (found %d), e.g. "Submitted batch job ([0-9]+)" — use (?:...) for grouping you don't want captured`,
			re.NumSubexp())
	}
	if c.KillTemplate == "" {
		return fmt.Errorf("kill_template is required")
	}
	return nil
}

// Presets returns the scheduler names WriteTemplate understands (for CLI help).
func Presets() []string { return []string{"slurm", "pbs", "sge"} }

// WriteTemplate creates the data dir and writes a starter config.yaml for the
// named scheduler ("slurm" | "pbs" | "sge"; empty = generic). It refuses to
// overwrite an existing file (returns the path and a flag so the CLI can report
// "already exists"). Presets are starting points — users edit for their site.
func WriteTemplate(scheduler string) (path string, created bool, err error) {
	tmpl, ok := templates[scheduler]
	if !ok {
		return "", false, fmt.Errorf("unknown scheduler %q (try: %v, or omit for generic)", scheduler, Presets())
	}
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path = ConfigPath()
	if _, err := os.Stat(path); err == nil {
		return path, false, nil // already exists, leave it alone
	}
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		return "", false, fmt.Errorf("write %s: %w", path, err)
	}
	return path, true, nil
}

// templates maps a scheduler name to its starter config.yaml. "" is the generic
// template. The header is shared; only the dialect-specific body differs.
var templates = map[string]string{
	"":      genericYAML,
	"slurm": slurmYAML,
	"pbs":   pbsYAML,
	"sge":   sgeYAML,
}

const configHeader = `# runq HPC config. runq does NOT parse scheduler dialects; it only interpolates
# these templates (shell-safe) and applies submit_id_regex. status_parser is an
# optional LIST of filter stages: runq feeds the raw status output to stage 1 on
# stdin and pipes each stage into the next; the final line must print one of:
#   pending | running | success | failed | killed | gone
# Regenerate for a specific scheduler with: runq hpc init --scheduler slurm|pbs|sge
`

// genericYAML: no scheduler assumed; status by status.json only until edited.
const genericYAML = configHeader + `
# Queue one task. Vars: {{run_sh}} {{gpus}} {{job_id}} {{task_id}} {{task_dir}}
submit_template: "sbatch --gpus={{gpus}} --job-name={{task_id}} {{run_sh}}"
submit_id_regex: "Submitted batch job ([0-9]+)"

# Optional scheduler probe (Var: {{ext_id}}). Empty → status from status.json alone.
status_template: ""
# Optional pipeline (each stage a shell filter; {{ext_id}} available).
status_parser: []

kill_template: "scancel {{ext_id}}"
`

// slurmYAML: sacct gives true terminal states; ParseSignal already knows its
// vocabulary, so the parser is just "take the first field of the first line"
// (handles e.g. "CANCELLED by 12345").
const slurmYAML = configHeader + `
submit_template: "sbatch --gpus={{gpus}} --job-name={{task_id}} {{run_sh}}"
submit_id_regex: "Submitted batch job ([0-9]+)"

status_template: "sacct -n -X -j {{ext_id}} -o State"
status_parser:
  - awk 'NR==1{print $1}'

kill_template: "scancel {{ext_id}}"
`

// pbsYAML: qstat -f exposes job_state as a single letter; the awk END block
// always emits a token (exit 0) per the status_parser contract.
// NOTE: this is an active-only query — when a job finishes, qstat errors on the
// unknown id, which runq treats as "no info" (status unchanged), so terminal
// states aren't detected here. For reliable terminal states use your PBS
// accounting query (the slurm preset shows the pattern with sacct).
// Starting point — adjust state letters for your PBS/Torque variant.
const pbsYAML = configHeader + `
submit_template: "qsub -l ngpus={{gpus}} -N {{task_id}} {{run_sh}}"
submit_id_regex: "([0-9]+)"

status_template: "qstat -f {{ext_id}} 2>/dev/null"
status_parser:
  - awk -F'= ' '/job_state/{print $2; f=1} END{if(!f) print "gone"}'
  - sed -e s/R/running/ -e s/E/running/ -e s/Q/pending/ -e s/H/pending/

kill_template: "qdel {{ext_id}}"
`

// sgeYAML: qstat lists active jobs (exit 0 even when ours is absent); awk matches
// our id, prints its state column, and emits gone via END otherwise (exit 0).
// Starting point — adjust columns/states for your SGE variant.
const sgeYAML = configHeader + `
submit_template: "qsub -l gpu={{gpus}} -N {{task_id}} {{run_sh}}"
submit_id_regex: "Your job ([0-9]+)"

status_template: "qstat"
status_parser:
  - awk -v id={{ext_id}} '$1==id{print $5; f=1} END{if(!f) print "gone"}'
  - sed -e s/r/running/ -e s/qw/pending/ -e s/Eqw/failed/

kill_template: "qdel {{ext_id}}"
`
