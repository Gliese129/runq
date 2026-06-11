// Package hpcconfig owns the HPC-specific section of the shared config file
// (~/.runq/config.yaml). It reads only the `hpc:` section; global keys
// (data_path, etc.) are owned by internal/config.
//
// Layout under the config dir (default ~/.runq):
//
//	~/.runq/
//	├── config.yaml          global + hpc config (this package reads `hpc:`)
//	└── runq.db              HPC store (reuses internal/store schema)
//
// Task workspaces are resolved by [config.ResolveRoot] and no longer live
// directly under ~/.runq.
package hpcconfig

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/gliese129/runq/internal/config"
	"gopkg.in/yaml.v3"
)

// Config is the `hpc:` section of ~/.runq/config.yaml. The cluster dialect
// lives entirely in these templates — runq does no Slurm/PBS/SGE parsing,
// only interpolation (via hpccore.Render) and one user-supplied regex.
type Config struct {
	// SubmitTemplate is the shell command that queues a job. Available vars:
	// {{run_sh}} {{gpus}} {{job_id}} {{task_id}} {{task_dir}} + {{param.*}}.
	SubmitTemplate string `yaml:"submit_template" json:"submit_template"`
	// SubmitIDRegex extracts the external job id from the submit command output.
	// Must contain exactly one capture group.
	SubmitIDRegex string `yaml:"submit_id_regex" json:"submit_id_regex"`
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
	StatusTemplate string `yaml:"status_template,omitempty" json:"status_template,omitempty"`
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
	StatusParser []string `yaml:"status_parser,omitempty" json:"status_parser,omitempty"`
	// KillTemplate cancels a queued/running job. Var: {{ext_id}}.
	KillTemplate string `yaml:"kill_template" json:"kill_template"`
	// PreflightLocal controls the local-subprocess preflight checks
	// (python imports, pip) on the submit node. nil/true = run them;
	// false = report them as skipped — for clusters whose login-node
	// policy forbids running user code there.
	PreflightLocal *bool `yaml:"preflight_local,omitempty" json:"preflight_local,omitempty"`
}

// configFile is the on-disk shape used for parsing. Only the HPC section is
// extracted here; global keys are handled by internal/config.
type configFile struct {
	HPC Config `yaml:"hpc"`
}

// DataDir delegates to [config.ConfigDir]. Kept as a convenience for HPC
// callers that need the DB path.
func DataDir() string { return config.ConfigDir() }

// DBPath is <DataDir>/runq.db (the HPC store, separate from the daemon DB).
func DBPath() string { return config.DBPath() }

// Load reads and validates the `hpc:` section of config.yaml. A missing file
// is a clear, actionable error pointing the user at `runq hpc init`.
func Load() (*Config, error) {
	path := config.ConfigPath()
	buf, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no HPC config at %s — run `runq hpc init` first", path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f configFile
	if err := yaml.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.HPC.validate(); err != nil {
		return nil, fmt.Errorf("invalid %s (hpc section): %w", path, err)
	}
	return &f.HPC, nil
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

// Save rewrites the hpc: section of config.yaml, preserving every other
// top-level key by value. NOTE: yaml round-tripping drops file comments —
// the GUI states this; CLI users keep `runq hpc config edit` which never
// touches the file beyond $EDITOR.
func Save(cfg *Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	path := config.ConfigPath()
	doc := map[string]any{}
	if buf, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(buf, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	doc["hpc"] = cfg
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.ConfigDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// Presets returns the scheduler names WriteTemplate understands (for CLI help).
func Presets() []string { return []string{"slurm", "pbs", "sge", "tsubame", "abci"} }

// WriteTemplate creates the config dir and writes a starter config.yaml for
// the named scheduler ("slurm" | "pbs" | "sge"; empty = generic).
//
// Three cases:
//   - no config.yaml exists         → write the full template (global + hpc)
//   - config.yaml exists WITHOUT hpc: → append the hpc section, preserving global keys
//   - config.yaml exists WITH hpc:    → refuse to overwrite ("already exists")
func WriteTemplate(scheduler string) (path string, created bool, err error) {
	tmpl, ok := templates[scheduler]
	if !ok {
		return "", false, fmt.Errorf("unknown scheduler %q (try: %v, or omit for generic)", scheduler, Presets())
	}
	hpcSnippet, ok := hpcSnippets[scheduler]
	if !ok {
		hpcSnippet = hpcSnippets[""]
	}

	dir := DataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path = config.ConfigPath()

	buf, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", false, fmt.Errorf("read %s: %w", path, readErr)
	}

	if os.IsNotExist(readErr) {
		// No file at all — write the full template.
		if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
			return "", false, fmt.Errorf("write %s: %w", path, err)
		}
		return path, true, nil
	}

	// File exists — check if it already has an hpc section.
	var probe struct {
		HPC yaml.Node `yaml:"hpc"`
	}
	if err := yaml.Unmarshal(buf, &probe); err != nil {
		return "", false, fmt.Errorf("parse existing %s: %w", path, err)
	}
	if probe.HPC.Kind != 0 {
		// Already has hpc: — leave it alone.
		return path, false, nil
	}

	// File exists but no hpc: section — append the hpc snippet.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", false, fmt.Errorf("open %s for append: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(hpcSnippet); err != nil {
		return "", false, fmt.Errorf("append hpc section to %s: %w", path, err)
	}
	return path, true, nil
}

// templates maps a scheduler name to its full starter config.yaml (for new files).
var templates = map[string]string{
	"":        genericYAML,
	"slurm":   slurmYAML,
	"pbs":     pbsYAML,
	"sge":     sgeYAML,
	"tsubame": tsubameYAML,
	"abci":    abciYAML,
}

// hpcSnippets maps a scheduler name to its hpc-only section (for appending to
// an existing config that has global keys but no hpc: section).
var hpcSnippets = map[string]string{
	"":        hpcHeader + genericHPCBody,
	"slurm":   hpcHeader + slurmHPCBody,
	"pbs":     hpcHeader + pbsHPCBody,
	"sge":     hpcHeader + sgeHPCBody,
	"tsubame": hpcHeader + tsubameHPCBody,
	"abci":    hpcHeader + abciHPCBody,
}

const configHeader = `# runq config — global settings + HPC cluster templates.
# Regenerate for a specific scheduler with: runq hpc init --scheduler slurm|pbs|sge

# ── Global (read by daemon + HPC) ──────────────────────────────────────────
# data_path: optional physical storage root for task workspaces. When set,
#   <working_dir>/.runq is a symlink to <data_path>/<project>/. When empty,
#   <working_dir>/.runq/ is the real storage location.
# data_path: "/scratch/user/runq"
`

const hpcHeader = `
# ── HPC cluster templates ──────────────────────────────────────────────────
# runq does NOT parse scheduler dialects; it only interpolates these templates
# (shell-safe) and applies submit_id_regex. status_parser is an optional LIST
# of filter stages: runq feeds the raw status output to stage 1 on stdin and
# pipes each stage into the next; the final line must print one of:
#   pending | running | success | failed | killed | gone
`

// HPC body constants — used both in full templates and in hpcSnippets.
const genericHPCBody = `
hpc:
  # Queue one task. Vars: {{run_sh}} {{gpus}} {{job_id}} {{task_id}} {{task_dir}}
  submit_template: "sbatch --gpus={{gpus}} --job-name={{task_id}} {{run_sh}}"
  submit_id_regex: "Submitted batch job ([0-9]+)"

  # Optional scheduler probe (Var: {{ext_id}}). Empty → status from status.json alone.
  status_template: ""
  # Optional pipeline (each stage a shell filter; {{ext_id}} available).
  status_parser: []

  kill_template: "scancel {{ext_id}}"
`

const slurmHPCBody = `
hpc:
  submit_template: "sbatch --gpus={{gpus}} --job-name={{task_id}} {{run_sh}}"
  submit_id_regex: "Submitted batch job ([0-9]+)"

  status_template: "sacct -n -X -j {{ext_id}} -o State"
  status_parser:
    - awk 'NR==1{print $1}'

  kill_template: "scancel {{ext_id}}"
`

const pbsHPCBody = `
hpc:
  submit_template: "qsub -l ngpus={{gpus}} -N {{task_id}} {{run_sh}}"
  submit_id_regex: "([0-9]+)"

  status_template: "qstat -f {{ext_id}} 2>/dev/null"
  status_parser:
    - awk -F'= ' '/job_state/{print $2; f=1} END{if(!f) print "gone"}'
    - sed -e s/R/running/ -e s/E/running/ -e s/Q/pending/ -e s/H/pending/

  kill_template: "qdel {{ext_id}}"
`

const sgeHPCBody = `
hpc:
  submit_template: "qsub -l gpu={{gpus}} -N {{task_id}} {{run_sh}}"
  submit_id_regex: "Your job ([0-9]+)"

  status_template: "qstat"
  status_parser:
    - awk -v id={{ext_id}} '$1==id{print $5; f=1} END{if(!f) print "gone"}'
    - sed -e s/r/running/ -e s/qw/pending/ -e s/Eqw/failed/

  kill_template: "qdel {{ext_id}}"
`

const tsubameHPCBody = `
hpc:
  # TSUBAME (UGE). Per-task scheduler knobs live in the sweep and are
  # referenced as {{param.*}} — zip node_kind / h_rt columns with your tasks.
  # $TSUBAME_GROUP comes from the login shell (or hardcode it).
  submit_template: "qsub -g $TSUBAME_GROUP -l {{param.node_kind}}=1 -l h_rt={{param.h_rt}} -N {{task_id}} -o {{task_dir}} -e {{task_dir}} {{run_sh}}"
  submit_id_regex: "Your job ([0-9]+)"

  status_template: "qstat"
  status_parser:
    - awk -v id={{ext_id}} '$1==id{print $5; f=1} END{if(!f) print "gone"}'
    - sed -e s/r/running/ -e s/qw/pending/ -e s/Eqw/failed/

  kill_template: "qdel {{ext_id}}"
`

const abciHPCBody = `
hpc:
  # ABCI (PBS Pro). Per-task knobs (queue, walltime) come from the sweep via
  # {{param.*}}. $ABCI_GROUP comes from the login shell (or hardcode it).
  submit_template: "qsub -P $ABCI_GROUP -q {{param.node_kind}} -l select=1 -l walltime={{param.walltime}} -N {{task_id}} -o {{task_dir}} -e {{task_dir}} -- {{run_sh}}"
  submit_id_regex: "([0-9]+)"

  status_template: "qstat -f {{ext_id}} 2>/dev/null"
  status_parser:
    - awk -F'= ' '/job_state/{print $2; f=1} END{if(!f) print "gone"}'
    - sed -e s/R/running/ -e s/E/running/ -e s/Q/pending/ -e s/H/pending/

  kill_template: "qdel {{ext_id}}"
`

// Full templates = global header + hpc header + hpc body (for new files).
var (
	genericYAML = configHeader + hpcHeader + genericHPCBody
	slurmYAML   = configHeader + hpcHeader + slurmHPCBody
	pbsYAML     = configHeader + hpcHeader + pbsHPCBody
	sgeYAML     = configHeader + hpcHeader + sgeHPCBody
	tsubameYAML = configHeader + hpcHeader + tsubameHPCBody
	abciYAML    = configHeader + hpcHeader + abciHPCBody
)

// Preset parses a named template's hpc section into a Config — the GUI's
// "load preset" uses this, so CLI init and GUI presets can never diverge.
func Preset(name string) (*Config, error) {
	body, ok := hpcSnippets[name]
	if !ok {
		return nil, fmt.Errorf("unknown preset %q (available: %s)", name, strings.Join(Presets(), ", "))
	}
	var f configFile
	if err := yaml.Unmarshal([]byte(body), &f); err != nil {
		return nil, fmt.Errorf("parse preset %q: %w", name, err)
	}
	return &f.HPC, nil
}
