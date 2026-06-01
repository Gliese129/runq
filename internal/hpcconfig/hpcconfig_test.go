package hpcconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)

	if got := DataDir(); got != dir {
		t.Fatalf("DataDir() = %q, want %q", got, dir)
	}
	if got, want := ConfigPath(), filepath.Join(dir, "config.yaml"); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
	if got, want := DBPath(), filepath.Join(dir, "runq.db"); got != want {
		t.Fatalf("DBPath() = %q, want %q", got, want)
	}
	if got, want := JobDir("abc"), filepath.Join(dir, "abc"); got != want {
		t.Fatalf("JobDir() = %q, want %q", got, want)
	}
}

func TestWriteTemplateThenLoad(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())

	path, created, err := WriteTemplate("")
	if err != nil {
		t.Fatalf("WriteTemplate: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on first write")
	}
	if path != ConfigPath() {
		t.Fatalf("path = %q, want %q", path, ConfigPath())
	}

	// Second call must not clobber.
	if _, created2, err := WriteTemplate(""); err != nil || created2 {
		t.Fatalf("second WriteTemplate: created=%v err=%v, want created=false err=nil", created2, err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SubmitTemplate == "" || cfg.SubmitIDRegex == "" || cfg.KillTemplate == "" {
		t.Fatalf("template missing required fields: %+v", cfg)
	}
}

func TestWriteTemplateSlurmPreset(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())
	if _, _, err := WriteTemplate("slurm"); err != nil {
		t.Fatalf("WriteTemplate(slurm): %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(cfg.StatusTemplate, "sacct") {
		t.Errorf("slurm preset should probe sacct, got %q", cfg.StatusTemplate)
	}
	if len(cfg.StatusParser) != 1 {
		t.Errorf("slurm preset status_parser = %v, want one stage", cfg.StatusParser)
	}
}

// Every shipped preset must be valid YAML and pass validation — guards against
// quoting mistakes in the awk/sed parser stages.
func TestAllPresetsParse(t *testing.T) {
	for _, sched := range append([]string{""}, Presets()...) {
		name := sched
		if name == "" {
			name = "generic"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("RUNQ_DATA_DIR", t.TempDir())
			if _, _, err := WriteTemplate(sched); err != nil {
				t.Fatalf("WriteTemplate(%q): %v", sched, err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load %q preset (YAML invalid?): %v", sched, err)
			}
			if cfg.SubmitTemplate == "" || cfg.KillTemplate == "" {
				t.Errorf("%q preset missing required fields", sched)
			}
		})
	}
}

func TestWriteTemplateUnknownScheduler(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", t.TempDir())
	if _, _, err := WriteTemplate("bogus"); err == nil {
		t.Fatal("expected error for unknown scheduler, got nil")
	}
}

func TestValidateSubmitIDRegex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	load := func(regex string) error {
		body := "submit_template: x\nsubmit_id_regex: '" + regex + "'\nkill_template: y\n"
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load()
		return err
	}
	if err := load(`([0-9`); err == nil {
		t.Error("expected error for invalid regex")
	}
	if err := load(`[0-9]+`); err == nil {
		t.Error("expected error for regex with no capture group")
	}
	if err := load(`(job) ([0-9]+)`); err == nil {
		t.Error("expected error for regex with two capture groups (ambiguous)")
	}
	if err := load(`job ([0-9]+)`); err != nil {
		t.Errorf("valid regex rejected: %v", err)
	}
	if err := load(`(?:job) ([0-9]+)`); err != nil {
		t.Errorf("non-capturing group should be allowed: %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("RUNQ_DATA_DIR", t.TempDir()) // empty dir, no config.yaml
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing config, got nil")
	}
}

func TestLoadInvalidMissingRequired(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNQ_DATA_DIR", dir)
	// submit_template omitted → validate must reject.
	body := "submit_id_regex: \"job ([0-9]+)\"\nkill_template: \"scancel {{ext_id}}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected validation error for missing submit_template, got nil")
	}
}
