package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/store"
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

// sampleConfig returns a Config with WorkingDir set to a real temp directory.
func sampleConfig(t *testing.T, name string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		ProjectName: name,
		WorkingDir:  dir,
		CmdTemplate: "python train.py {{args}}",
		Defaults:    Defaults{GPUsPerTask: 1, MaxRetry: 3},
		Resume:      ResumeConfig{Enabled: true, ExtraArgs: "--resume"},
	}
}

func TestAddAndGet(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig(t, "resnet50")
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// project.yaml must exist on disk
	yamlPath := filepath.Join(cfg.WorkingDir, "project.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf("project.yaml not written: %v", err)
	}

	got, err := reg.Get(context.Background(), "resnet50")
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

func TestAddFailsOnBadDir(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := Config{
		ProjectName: "bad",
		WorkingDir:  "/this/path/does/not/exist",
		CmdTemplate: "echo {{args}}",
	}
	err := reg.Add(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for non-existent working_dir, got nil")
	}

	// DB should NOT have the project
	_, getErr := reg.Get(context.Background(), "bad")
	if getErr == nil {
		t.Fatal("project should not be in DB after failed Add")
	}
}

func TestAddDuplicate(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig(t, "resnet50")
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("first Add failed: %v", err)
	}
	err := reg.Add(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for duplicate project, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
}

func TestAddSkipsWriteIfYamlExists(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig(t, "existing")
	// Pre-create a project.yaml with custom content
	yamlPath := filepath.Join(cfg.WorkingDir, "project.yaml")
	original := []byte("# my custom yaml\nproject_name: existing\n")
	if err := os.WriteFile(yamlPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// File should be untouched (not overwritten)
	got, _ := os.ReadFile(yamlPath)
	if string(got) != string(original) {
		t.Error("project.yaml was overwritten; WriteYAML should skip existing files")
	}
}

func TestGetNotFound(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	_, err := reg.Get(context.Background(), "nonexistent")
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
		if err := reg.Add(context.Background(), sampleConfig(t, name)); err != nil {
			t.Fatalf("Add %q failed: %v", name, err)
		}
	}

	configs, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(configs) != 3 {
		t.Fatalf("expected 3 projects, got %d", len(configs))
	}
	if configs[0].ProjectName != "bert" || configs[2].ProjectName != "resnet50" {
		t.Errorf("unexpected order: %v, %v, %v",
			configs[0].ProjectName, configs[1].ProjectName, configs[2].ProjectName)
	}
}

func TestListEmpty(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	configs, err := reg.List(context.Background())
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

	cfg := sampleConfig(t, "resnet50")
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	cfg.CmdTemplate = "python train_v2.py {{args}}"
	cfg.Defaults.MaxRetry = 5
	if err := reg.Update(context.Background(), cfg); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, err := reg.Get(context.Background(), "resnet50")
	if err != nil {
		t.Fatalf("Get after Update failed: %v", err)
	}
	if got.CmdTemplate != "python train_v2.py {{args}}" {
		t.Errorf("cmd = %q, want updated value", got.CmdTemplate)
	}
	if got.Defaults.MaxRetry != 5 {
		t.Errorf("max_retry = %d, want 5", got.Defaults.MaxRetry)
	}

	// project.yaml should be overwritten by Update
	data, _ := os.ReadFile(filepath.Join(cfg.WorkingDir, "project.yaml"))
	if !strings.Contains(string(data), "train_v2.py") {
		t.Error("project.yaml not updated after Update")
	}
}

func TestUpdateNotFound(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	cfg := sampleConfig(t, "nonexistent")
	err := reg.Update(context.Background(), cfg)
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

	cfg := sampleConfig(t, "resnet50")
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := reg.Remove(context.Background(), "resnet50"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	_, err := reg.Get(context.Background(), "resnet50")
	if err == nil {
		t.Fatal("expected error after Remove, got nil")
	}
}

func TestRemoveNotFound(t *testing.T) {
	reg, cleanup := setup(t)
	defer cleanup()

	err := reg.Remove(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for removing missing project, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}
