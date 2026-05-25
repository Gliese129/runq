package project

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWandbConfigOptional verifies that omitting the wandb block leaves
// Config.Wandb as nil — the daemon uses nil-ness to decide whether to write
// wandb_config.json for each task.
func TestWandbConfigOptional(t *testing.T) {
	yml := `
project_name: foo
working_dir: /tmp
command_template: echo {{args}}
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Wandb != nil {
		t.Errorf("expected Wandb nil, got %+v", cfg.Wandb)
	}
}

// TestWandbConfigPresent verifies all fields parse correctly when wandb is
// configured, including tags slice and the offline mode value.
func TestWandbConfigPresent(t *testing.T) {
	yml := `
project_name: foo
working_dir: /tmp
command_template: echo {{args}}
wandb:
  project: my-exp
  entity: my-team
  tags: [vision, baseline]
  mode: offline
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Wandb == nil {
		t.Fatal("expected Wandb non-nil")
	}
	if cfg.Wandb.Project != "my-exp" {
		t.Errorf("project: got %q, want %q", cfg.Wandb.Project, "my-exp")
	}
	if cfg.Wandb.Entity != "my-team" {
		t.Errorf("entity: got %q, want %q", cfg.Wandb.Entity, "my-team")
	}
	if got := cfg.Wandb.Tags; len(got) != 2 || got[0] != "vision" || got[1] != "baseline" {
		t.Errorf("tags: got %v, want [vision baseline]", got)
	}
	if cfg.Wandb.Mode != "offline" {
		t.Errorf("mode: got %q, want %q", cfg.Wandb.Mode, "offline")
	}
}

// TestWandbConfigPartial — only project given, rest defaults. Daemon must
// transparently forward whatever the user provides without validation.
func TestWandbConfigPartial(t *testing.T) {
	yml := `
project_name: foo
working_dir: /tmp
command_template: echo {{args}}
wandb:
  project: minimal
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(yml), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Wandb == nil || cfg.Wandb.Project != "minimal" {
		t.Fatalf("expected Wandb.Project=minimal, got %+v", cfg.Wandb)
	}
	if cfg.Wandb.Entity != "" || cfg.Wandb.Mode != "" || len(cfg.Wandb.Tags) != 0 {
		t.Errorf("optional fields should default to empty, got %+v", cfg.Wandb)
	}
}
