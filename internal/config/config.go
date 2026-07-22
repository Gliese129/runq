// Package config owns the global runq configuration (~/.runq/config.yaml) and
// the unified storage-path resolution used by every backend (daemon and HPC).
//
// The global config lives at <ConfigDir>/config.yaml where ConfigDir is:
//   - RUNQ_DATA_DIR (env override), else
//   - ~/.runq (always per-user; the daemon's *runtime* dir is separate)
//
// The file has two sections: top-level keys are global (read by all backends),
// remote-target templates live on each targets[] entry (internal/remote).
//
// # Storage model
//
// Every task's workspace (params.json, metrics.jsonl, checkpoints/, logs) lives
// under a single root resolved by [ResolveRoot]:
//
//	<root>/<job_id>/<task_id>/
//
// Root resolution ([ResolveRoot] always returns the PHYSICAL path):
//   - data_path configured → returns <data_path>/<project>/
//     plus a convenience symlink: <working_dir>/.runq → <data_path>/<project>
//   - data_path empty       → returns <working_dir>/.runq/
//
// The physical path is what gets persisted in the DB (task_dir), so config
// changes never orphan running tasks. Both daemon and HPC call ResolveRoot;
// downstream code (workspace.Write, ingest.ReapIncremental, submitplan.Build) is
// unaware of which mode is active.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gliese129/runq/internal/genfile"
)

// GlobalConfig is the top-level section of ~/.runq/config.yaml.
// Fields added here are visible to every backend.
type GlobalConfig struct {
	// DataPath is the optional physical storage root. When set, task artifacts
	// are stored under <DataPath>/<project>/<job_id>/<task_id>/ and the
	// project's <working_dir>/.runq is a symlink pointing there. When empty,
	// <working_dir>/.runq/ is the real storage location.
	DataPath string `yaml:"data_path,omitempty"`
	// DefaultTarget names the target to use when --target is omitted.
	// Falls back to the first entry in Targets, then "local".
	DefaultTarget string `yaml:"default_target,omitempty"`
	// Dashboard configures the embedded dashboard server.
	Dashboard *DashboardConfig `yaml:"dashboard,omitempty"`
	// Targets lists the compute backends this instance manages.
	// Target type is inferred from fields: gpus → local, scheduler → HPC.
	// SSH section determines filesystem: present → SSHFS, absent → LocalFS.
	Targets []TargetConfig `yaml:"targets,omitempty"`

	// Generation is the SEMANTIC content hash of config.yaml at Load time
	// (RQ-75, computed by genfile — reformatting and comments do not move
	// it). Never serialized: it describes the file, it does not live in it.
	// Empty for a missing config file.
	Generation string `yaml:"-" json:"-"`
}

// ── Target configuration ───────────────────────────────────────────────────

// DashboardConfig controls the embedded dashboard HTTP server.
type DashboardConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen,omitempty"` // default "127.0.0.1:8077"
}

// TargetConfig describes a single compute target. Type is inferred:
//   - gpus present   → LocalBackend (daemon-managed GPU scheduling)
//   - scheduler set  → SSHBackend   (cluster job submission via SSH)
//   - ssh present    → SSHFS        (remote filesystem); absent → LocalFS
type TargetConfig struct {
	Name      string           `yaml:"name" json:"name"`
	GPUs      []int            `yaml:"gpus,omitempty" json:"gpus,omitempty"`
	Scheduler string           `yaml:"scheduler,omitempty" json:"scheduler,omitempty"` // e.g. "slurm", "pbs"
	Workspace string           `yaml:"workspace,omitempty" json:"workspace,omitempty"` // HPC workspace root on the target
	SSH       *SSHTargetConfig `yaml:"ssh,omitempty" json:"ssh,omitempty"`

	// MaxInflight caps how many of this target's tasks may be in flight on
	// the external scheduler at once (submitted and not yet terminal); tasks
	// beyond the cap wait in runq's own queue. Protects against per-user
	// submit limits (e.g. Slurm MaxSubmitJobs) on huge sweeps, and keeps
	// not-yet-submitted tasks cancellable at zero cost. 0 = unlimited.
	MaxInflight int `yaml:"max_inflight,omitempty" json:"max_inflight,omitempty"`

	// RemoteCLI enables the reverse socket forward for this target: the
	// daemon keeps ~/.runq/runq.sock listening on the remote host, so a
	// `runq` CLI there (installed by `runq connect`) talks to THIS daemon —
	// users whose code lives on the login node get the full CLI without a
	// second deployment. The forward lives and dies with the daemon; when
	// this machine is offline the remote CLI reports the tunnel as down.
	RemoteCLI bool `yaml:"remote_cli,omitempty" json:"remote_cli,omitempty"`

	// TrustEmptyList declares that an EMPTY status_list output is a real
	// answer ("no jobs"), not a parse suspicion. runqd targets set it (their
	// squeue reads a local SQLite); dialect schedulers leave it off so the
	// conservative per-job fallback still guards against silent breakage.
	TrustEmptyList bool `yaml:"trust_empty_list,omitempty" json:"trust_empty_list,omitempty"`

	// DoneDir overrides the done-marker directory (default:
	// <workspace>/.runq-done). The client's synthesized localhost-runqd lane
	// sets this to a data-dir location so marker-based completion works
	// without forcing a central workspace root onto local projects.
	DoneDir string `yaml:"done_dir,omitempty" json:"done_dir,omitempty"`

	// RunqBin is the ABSOLUTE path of the runq binary on the target
	// (install-on-*.sh puts it there). When set, run.sh gains compute-node
	// work that needs runq locally — currently the metrics pyramid build
	// (`metrics-index build`) before the done marker. An absolute path, not
	// PATH: batch-job environments are minimal and non-interactive shells
	// don't source the user's profile.
	RunqBin string `yaml:"runq_bin,omitempty" json:"runq_bin,omitempty"`

	// GPUTemplate is an optional command whose output is a JSON []GPUSlot —
	// this target's GPU view for the client's aggregated (local ∪ remote)
	// dashboard panel. The runq preset sets `runq gpu --json`; schedulers
	// without a meaningful per-user GPU view (slurm login nodes) leave it
	// empty and simply contribute nothing to the panel.
	GPUTemplate string `yaml:"gpu_template,omitempty" json:"gpu_template,omitempty"`

	// ── HPC scheduler templates (scheduler-type targets only) ──────────────

	// SubmitTemplate is the shell command that queues a task.
	// Vars: {{run_sh}} {{gpus}} {{job_id}} {{task_id}} {{task_dir}} {{name}} {{param.*}}.
	SubmitTemplate string `yaml:"submit_template,omitempty" json:"submit_template,omitempty"`
	// SubmitIDRegex extracts the external job ID from submit output.
	// Must contain exactly one capture group.
	SubmitIDRegex string `yaml:"submit_id_regex,omitempty" json:"submit_id_regex,omitempty"`
	// StatusTemplate probes a single task's scheduler state. Var: {{ext_id}}.
	StatusTemplate string `yaml:"status_template,omitempty" json:"status_template,omitempty"`
	// StatusParser is an optional pipeline that normalizes status_template output.
	StatusParser []string `yaml:"status_parser,omitempty" json:"status_parser,omitempty"`
	// StatusListTemplate is a batch probe returning ALL user jobs at once.
	StatusListTemplate string `yaml:"status_list_template,omitempty" json:"status_list_template,omitempty"`
	// StatusListParser normalizes batch probe output into "ext_id signal" lines.
	StatusListParser []string `yaml:"status_list_parser,omitempty" json:"status_list_parser,omitempty"`
	// SignalMap maps scheduler-native tokens to canonical signals.
	SignalMap map[string]string `yaml:"signal_map,omitempty" json:"signal_map,omitempty"`
	// KillTemplate cancels a queued/running job. Var: {{ext_id}}.
	KillTemplate string `yaml:"kill_template,omitempty" json:"kill_template,omitempty"`
	// PreflightLocal controls whether local preflight checks run on the submit node.
	PreflightLocal *bool `yaml:"preflight_local,omitempty" json:"preflight_local,omitempty"`
}

// SSHTargetConfig holds SSH connection parameters for a remote target.
type SSHTargetConfig struct {
	Host      string `yaml:"host" json:"host"`
	User      string `yaml:"user" json:"user"`
	Key       string `yaml:"key,omitempty" json:"key,omitempty"`               // path to private key; empty → agent
	Port      int    `yaml:"port,omitempty" json:"port,omitempty"`             // default 22
	ProxyJump string `yaml:"proxy_jump,omitempty" json:"proxy_jump,omitempty"` // SSH ProxyJump host
}

// Target type constants — the WIRE vocabulary (spec §4: local | remote).
// "remote" covers every scheduler/ssh target; runqd-vs-slurm is a dialect
// detail (TargetSummary.Scheduler), never a type.
const (
	TargetTypeLocal  = "local"
	TargetTypeRemote = "remote"
)

// Type returns the inferred backend type for this target.
func (t *TargetConfig) Type() string {
	if t.Scheduler != "" {
		return TargetTypeRemote
	}
	return TargetTypeLocal
}

// IsRemote reports whether this target uses SSH (remote filesystem).
func (t *TargetConfig) IsRemote() bool {
	return t.SSH != nil
}

// ResolveTargets returns the configured targets. An empty targets[] means
// "just this machine": a single default local target. (mode is dead, D9 —
// a stale `mode:` key in old config files parses as an ignored unknown.)
func (cfg *GlobalConfig) ResolveTargets() []TargetConfig {
	if len(cfg.Targets) > 0 {
		return cfg.Targets
	}
	return []TargetConfig{{Name: "local"}}
}

// ResolveDefaultTarget returns the name of the default target.
func (cfg *GlobalConfig) ResolveDefaultTarget() string {
	if cfg.DefaultTarget != "" {
		return cfg.DefaultTarget
	}
	targets := cfg.ResolveTargets()
	if len(targets) > 0 {
		return targets[0].Name
	}
	return "local"
}

// FindTarget looks up a target by name. Returns an error if not found.
func (cfg *GlobalConfig) FindTarget(name string) (*TargetConfig, error) {
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == name {
			return &cfg.Targets[i], nil
		}
	}
	return nil, fmt.Errorf("target %q not found in config", name)
}

// configFile is the on-disk shape: global keys at the top, plus an opaque hpc
// section that this package does NOT parse (hpcconfig owns that).
type configFile struct {
	GlobalConfig `yaml:",inline"`
}

// SaveGlobal writes the entire GlobalConfig back to config.yaml, preserving
// the hpc: section if present.
// CheckConfigPermissions enforces the RQ-45 startup gate on config.yaml:
// world-writable is a refusal (anyone on the machine could inject ssh
// hosts or submit templates the daemon will EXECUTE); group-writable is a
// warning. A missing file is fine — nothing to tamper with.
func CheckConfigPermissions() (warning string, err error) {
	fi, statErr := os.Stat(ConfigPath())
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", nil
		}
		return "", statErr
	}
	mode := fi.Mode().Perm()
	if mode&0o002 != 0 {
		return "", fmt.Errorf("%s is world-writable (%s) — refusing to start; fix with: chmod 600 %s", ConfigPath(), mode, ConfigPath())
	}
	if mode&0o020 != 0 {
		warning = fmt.Sprintf("%s is group-writable (%s) — consider chmod 600", ConfigPath(), mode)
	}
	return warning, nil
}

func SaveGlobal(cfg *GlobalConfig) error {
	return saveGlobalWith(cfg, "")
}

// SaveGlobalIfMatch persists cfg with optimistic concurrency (RQ-75): the
// file's current SEMANTIC generation must equal ifMatch or the save fails
// with *genfile.ConflictError carrying the current generation. Empty
// ifMatch degrades to the unconditional SaveGlobal (legacy CLI writers).
func SaveGlobalIfMatch(cfg *GlobalConfig, ifMatch string) error {
	return saveGlobalWith(cfg, ifMatch)
}

func saveGlobalWith(cfg *GlobalConfig, ifMatch string) error {
	out, err := renderGlobalYAML(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir(), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// genfile.Save: flock + generation compare + tmp/rename atomic replace.
	// Even the unconditional path gains atomicity over the old WriteFile.
	return genfile.Save(ConfigPath(), out, ifMatch, 0o644)
}

// renderGlobalYAML builds config.yaml's new bytes by SURGICALLY updating
// the current file's node tree (SetKey's technique, extended to the whole
// managed set): comments and unmanaged sections (hpc:) outside the replaced
// keys survive an API write. Comments INSIDE a replaced section (targets)
// are necessarily lost when that section changes — a form edit rewrites it.
func renderGlobalYAML(cfg *GlobalConfig) ([]byte, error) {
	doc, err := readConfigNode()
	if err != nil {
		return nil, err
	}
	root := doc.Content[0]
	// Scalars are authoritative like targets (review fix #3): callers
	// always start from Load(), so an empty value means absent-or-cleared
	// — remove the key instead of silently keeping the disk value.
	if cfg.DefaultTarget != "" {
		setMappingScalar(root, "default_target", cfg.DefaultTarget)
	} else {
		removeMappingValue(root, "default_target")
	}
	if cfg.DataPath != "" {
		setMappingScalar(root, "data_path", cfg.DataPath)
	} else {
		removeMappingValue(root, "data_path")
	}
	if cfg.Dashboard != nil {
		n, nerr := encodeNode(cfg.Dashboard)
		if nerr != nil {
			return nil, nerr
		}
		setMappingNode(root, "dashboard", n)
	}
	// Targets are authoritative on every save: callers always start from
	// Load(), so an empty list MEANS "no targets" — remove the key. (The
	// old map-overlay skipped empty lists, which made deleting the last
	// target a silent no-op.)
	if len(cfg.Targets) > 0 {
		n, nerr := encodeNode(cfg.Targets)
		if nerr != nil {
			return nil, nerr
		}
		setMappingNode(root, "targets", n)
	} else {
		removeMappingValue(root, "targets")
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return out, nil
}

// encodeNode marshals v into a yaml node for surgical insertion.
func encodeNode(v any) (*yaml.Node, error) {
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return nil, fmt.Errorf("encode config section: %w", err)
	}
	return n, nil
}

// setMappingNode sets key to an arbitrary node value (setMappingScalar's
// sibling for non-scalar sections).
func setMappingNode(root *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = val
			return
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val,
	)
}

// ConfigDir resolves the directory that holds config.yaml (and, for HPC, the
// DB and job workspaces). RUNQ_DATA_DIR overrides; default is ~/.runq.
func ConfigDir() string {
	if dir := os.Getenv("RUNQ_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".runq")
}

// ConfigPath is <ConfigDir>/config.yaml.
func ConfigPath() string { return filepath.Join(ConfigDir(), "config.yaml") }

// DBPath is <ConfigDir>/runq.db (the HPC store; the daemon has its own DB
// under its runtime data dir).
func DBPath() string { return filepath.Join(ConfigDir(), "runq.db") }

// Load reads the global section of config.yaml. A missing file is not an error
// — it returns a zero GlobalConfig (data_path empty = project_path mode).
// The returned config carries its semantic Generation (RQ-75).
func Load() (*GlobalConfig, error) {
	buf, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return &GlobalConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ConfigPath(), err)
	}
	var f configFile
	if err := yaml.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(), err)
	}
	// Same bytes we just parsed — the generation cannot disagree with the
	// content this config was built from.
	if gen, gerr := genfile.SemanticHash(buf); gerr == nil {
		f.GlobalConfig.Generation = gen
	}
	return &f.GlobalConfig, nil
}

// Keys returns the supported top-level global config keys.
func Keys() []string {
	return []string{"data_path", "default_target"}
}

// GetKey reads one supported global config key.
func GetKey(key string) (string, error) {
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	switch key {
	case "data_path":
		return cfg.DataPath, nil
	case "default_target":
		return cfg.ResolveDefaultTarget(), nil
	default:
		return "", fmt.Errorf("unsupported config key %q", key)
	}
}

// ListKeys returns all supported global config keys with normalized values.
func ListKeys() (map[string]string, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"data_path":      cfg.DataPath,
		"default_target": cfg.ResolveDefaultTarget(),
	}, nil
}

// SetKey updates one top-level global config key while preserving unrelated
// YAML sections such as hpc:. Empty data_path removes that key.
func SetKey(key, value string) error {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	switch key {
	case "data_path":
		// No extra validation: users may point at paths not mounted on this host.
	case "default_target":
		// Accept any name; actual validation happens when targets are resolved.
	default:
		return fmt.Errorf("unsupported config key %q", key)
	}

	doc, err := readConfigNode()
	if err != nil {
		return err
	}
	root := doc.Content[0]
	if key == "data_path" && value == "" {
		removeMappingValue(root, key)
	} else {
		setMappingScalar(root, key, value)
	}
	if err := os.MkdirAll(ConfigDir(), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(ConfigPath(), out, 0o644)
}

func readConfigNode() (*yaml.Node, error) {
	buf, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return newConfigNode(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ConfigPath(), err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(buf, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(), err)
	}
	if len(doc.Content) == 0 {
		return newConfigNode(), nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %s: config root must be a YAML mapping", ConfigPath())
	}
	return &doc, nil
}

func newConfigNode() *yaml.Node {
	return &yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{{
			Kind: yaml.MappingNode,
		}},
	}
}

func setMappingScalar(root *yaml.Node, key, value string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func removeMappingValue(root *yaml.Node, key string) {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			return
		}
	}
}

// ResolveRoot returns the PHYSICAL workspace root for a project. Both daemon
// and HPC use this to compute submitplan.Paths.WorkspaceRoot. The returned
// path is what gets persisted in the DB (task_dir), so it must be stable
// across config changes.
//
// When data_path is set it also creates a convenience symlink:
//
//	<working_dir>/.runq  →  <data_path>/<project>/
//
// but the returned (and persisted) path is always the physical location, not
// the symlink. This way, if data_path changes later, old task rows still
// point to their real directory.
// ProspectiveRoot returns where ResolveRoot WOULD place the workspace,
// creating NOTHING — previews must stay zero-disk. Mirrors ResolveRoot's
// path logic only (no mkdir, no symlink, no validation side effects).
func ProspectiveRoot(cfg *GlobalConfig, workingDir, project string) string {
	if cfg == nil || cfg.DataPath == "" {
		return filepath.Join(workingDir, ".runq")
	}
	return filepath.Join(cfg.DataPath, project)
}

func ResolveRoot(cfg *GlobalConfig, workingDir, project string) (string, error) {
	if err := validateProjectName(project); err != nil {
		return "", err
	}

	projectPath := filepath.Join(workingDir, ".runq")

	if cfg == nil || cfg.DataPath == "" {
		// project_path mode: no indirection; projectPath IS the physical root.
		return projectPath, nil
	}

	// data_path mode: physical storage at <data_path>/<project>/.
	realPath := filepath.Join(cfg.DataPath, project)
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		return "", fmt.Errorf("create data_path dir %s: %w", realPath, err)
	}

	// Best-effort convenience symlink; a failure here is not fatal — the
	// physical path is what matters (DB stores the physical path, not the
	// symlink). Silent: the caller (CLI/daemon) decides whether to surface it.
	_ = ensureSymlink(projectPath, realPath)

	// Return the PHYSICAL path — this is what goes into the DB.
	return realPath, nil
}

// validateProjectName rejects project names that would escape the intended
// storage directory: path separators, dot-segments, absolute paths, empty.
func validateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name must not be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("project name %q is a dot-segment", name)
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("project name %q must not be an absolute path", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("project name %q must not contain path separators", name)
	}
	return nil
}

// ensureSymlink makes sure `link` is a symlink pointing at `target`.
//
// Cases:
//   - link does not exist      → create symlink
//   - link is already correct  → no-op
//   - link points elsewhere    → remove and recreate (config changed)
//   - link is a real directory → error (user must migrate manually)
func ensureSymlink(link, target string) error {
	// Ensure the parent directory of the symlink exists.
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create parent for symlink %s: %w", link, err)
	}

	fi, err := os.Lstat(link)
	if os.IsNotExist(err) {
		return os.Symlink(target, link)
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", link, err)
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		// Already a symlink — check if it points to the right place.
		cur, err := os.Readlink(link)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", link, err)
		}
		if cur == target {
			return nil // correct
		}
		// Points elsewhere (config changed): re-point.
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("remove stale symlink %s: %w", link, err)
		}
		return os.Symlink(target, link)
	}

	// It's a real directory (pre-existing data from before data_path was set).
	return fmt.Errorf(
		"%s is an existing directory, not a symlink — move its contents to %s manually, "+
			"then remove the directory so runq can create the symlink",
		link, target)
}
