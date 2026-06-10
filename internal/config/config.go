// Package config owns the global runq configuration (~/.runq/config.yaml) and
// the unified storage-path resolution used by every backend (daemon and HPC).
//
// The global config lives at <ConfigDir>/config.yaml where ConfigDir is:
//   - RUNQ_DATA_DIR (env override), else
//   - ~/.runq (always per-user; the daemon's *runtime* dir is separate)
//
// The file has two sections: top-level keys are global (read by all backends),
// and the `hpc:` section is HPC-specific (parsed by internal/hpcconfig).
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
// downstream code (workspace.Write, ingest.ReapOutputs, submitplan.Build) is
// unaware of which mode is active.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// GlobalConfig is the top-level section of ~/.runq/config.yaml.
// Fields added here are visible to every backend.
type GlobalConfig struct {
	// DataPath is the optional physical storage root. When set, task artifacts
	// are stored under <DataPath>/<project>/<job_id>/<task_id>/ and the
	// project's <working_dir>/.runq is a symlink pointing there. When empty,
	// <working_dir>/.runq/ is the real storage location.
	DataPath string `yaml:"data_path,omitempty"`
	// Mode selects the default backend for unified CLI/dashboard commands.
	// Empty is accepted on disk and normalized to daemon when loaded.
	Mode string `yaml:"mode,omitempty"`
}

const (
	ModeDaemon = "daemon"
	ModeHPC    = "hpc"
)

// configFile is the on-disk shape: global keys at the top, plus an opaque hpc
// section that this package does NOT parse (hpcconfig owns that).
type configFile struct {
	GlobalConfig `yaml:",inline"`
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
func Load() (*GlobalConfig, error) {
	buf, err := os.ReadFile(ConfigPath())
	if os.IsNotExist(err) {
		return &GlobalConfig{Mode: ModeDaemon}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ConfigPath(), err)
	}
	var f configFile
	if err := yaml.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(), err)
	}
	mode, err := NormalizeMode(f.GlobalConfig.Mode)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(), err)
	}
	f.GlobalConfig.Mode = mode
	return &f.GlobalConfig, nil
}

// RawMode reports the mode value as literally present in config.yaml,
// and whether it was explicitly set at all. Load() normalizes "" → daemon,
// which is right for consumers but erases the "never chosen" state that
// `hpc init` needs (it only flips the default, never an explicit choice).
func RawMode() (mode string, explicit bool) {
	buf, err := os.ReadFile(ConfigPath())
	if err != nil {
		return "", false
	}
	var f configFile
	if yaml.Unmarshal(buf, &f) != nil {
		return "", false
	}
	raw := strings.TrimSpace(f.GlobalConfig.Mode)
	return raw, raw != ""
}

// NormalizeMode returns the canonical mode value accepted by the unified CLI.
func NormalizeMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return ModeDaemon, nil
	}
	switch mode {
	case ModeDaemon, ModeHPC:
		return mode, nil
	default:
		return "", fmt.Errorf("mode must be %q or %q, got %q", ModeDaemon, ModeHPC, mode)
	}
}

// ConfigMode returns cfg.Mode with nil/empty values treated as daemon.
func ConfigMode(cfg *GlobalConfig) string {
	if cfg == nil {
		return ModeDaemon
	}
	mode, err := NormalizeMode(cfg.Mode)
	if err != nil {
		return ModeDaemon
	}
	return mode
}

// Keys returns the supported top-level global config keys.
func Keys() []string {
	return []string{"mode", "data_path"}
}

// GetKey reads one supported global config key.
func GetKey(key string) (string, error) {
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	switch key {
	case "mode":
		return ConfigMode(cfg), nil
	case "data_path":
		return cfg.DataPath, nil
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
		"mode":      ConfigMode(cfg),
		"data_path": cfg.DataPath,
	}, nil
}

// SetKey updates one top-level global config key while preserving unrelated
// YAML sections such as hpc:. Empty data_path removes that key.
func SetKey(key, value string) error {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	switch key {
	case "mode":
		mode, err := NormalizeMode(value)
		if err != nil {
			return err
		}
		value = mode
	case "data_path":
		// No extra validation: users may point at paths not mounted on this host.
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
