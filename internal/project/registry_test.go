package project

import (
	"strings"
	"testing"

	"github.com/runq/runq/internal/store"
)

// helper: open an in-memory store and return a Registry + cleanup func.
func setup(t *testing.T) (*Registry, func()) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	reg := NewRegistry(s.DB())
	return reg, func() { s.Close() }
}

func sampleConfig(name string) Config {
	return Config{
		ProjectName: name,
		WorkingDir:  "/home/user/projects/" + name,
		CmdTemplate: "python train.py {{args}}",
		Defaults:    Defaults{GPUsPerTask: 1, MaxRetry: 3},
		Resume:      ResumeConfig{Enabled: true, ExtraArgs: "--resume"},
	}
}

func TestAddAndGet(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig("resnet50")
	if err := reg.Add(cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	got, err := reg.Get("resnet50")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ProjectName != "resnet50" {
		t.Errorf("name = %q, want %q", got.ProjectName, "resnet50")
	}
	if got.CmdTemplate != cfg.CmdTemplate {
		t.Errorf("cmd = %q, want %q", got.CmdTemplate, cfg.CmdTemplate)
	}
	if got.Defaults.MaxRetry != 3 {
		t.Errorf("max_retry = %d, want 3", got.Defaults.MaxRetry)
	}
	if !got.Resume.Enabled {
		t.Error("resume.enabled should be true")
	}
}

func TestAddDuplicate(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig("resnet50")
	if err := reg.Add(cfg); err != nil {
		t.Fatalf("first Add failed: %v", err)
	}
	err := reg.Add(cfg)
	if err == nil {
		t.Fatal("expected error for duplicate project, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	_, err := reg.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing project, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestList(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	for _, name := range []string{"bert", "gpt2", "resnet50"} {
		if err := reg.Add(sampleConfig(name)); err != nil {
			t.Fatalf("Add %q failed: %v", name, err)
		}
	}

	configs, err := reg.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(configs) != 3 {
		t.Fatalf("expected 3 projects, got %d", len(configs))
	}
	// Should be ordered by name
	if configs[0].ProjectName != "bert" || configs[2].ProjectName != "resnet50" {
		t.Errorf("unexpected order: %v, %v, %v",
			configs[0].ProjectName, configs[1].ProjectName, configs[2].ProjectName)
	}
}

func TestListEmpty(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	configs, err := reg.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("expected 0 projects, got %d", len(configs))
	}
}

func TestUpdate(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig("resnet50")
	if err := reg.Add(cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	cfg.CmdTemplate = "python train_v2.py {{args}}"
	cfg.Defaults.MaxRetry = 5
	if err := reg.Update(cfg); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, err := reg.Get("resnet50")
	if err != nil {
		t.Fatalf("Get after Update failed: %v", err)
	}
	if got.CmdTemplate != "python train_v2.py {{args}}" {
		t.Errorf("cmd = %q, want updated value", got.CmdTemplate)
	}
	if got.Defaults.MaxRetry != 5 {
		t.Errorf("max_retry = %d, want 5", got.Defaults.MaxRetry)
	}
}

func TestUpdateNotFound(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	err := reg.Update(sampleConfig("nonexistent"))
	if err == nil {
		t.Fatal("expected error for updating missing project, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestRemove(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig("resnet50")
	if err := reg.Add(cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := reg.Remove("resnet50"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	_, err := reg.Get("resnet50")
	if err == nil {
		t.Fatal("expected error after Remove, got nil")
	}
}

func TestRemoveNotFound(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	err := reg.Remove("nonexistent")
	if err == nil {
		t.Fatal("expected error for removing missing project, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}
