package preflight

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/project"
)

// Codex r1 #2a: a `bash run.sh` entry is scanned for the python it
// invokes — the imports of THAT script are probed, not skipped.
func TestShellEntryPythonRecursion(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	writeF := func(name, content string) {
		if err := os.WriteFile(dir+"/"+name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeF("run.sh", "#!/bin/sh\n"+interp+" train.py --x 1\n")
	writeF("train.py", "import json\nimport definitely_not_a_module_zz\n")
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "bash run.sh"}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, "bash run.sh", &report)
	statuses := map[string]CheckResult{}
	for _, c := range results {
		statuses[c.Name] = c
	}
	imp := statuses["imports"]
	if imp.Status != "failed" || !strings.Contains(imp.Detail, "definitely_not_a_module_zz") {
		t.Fatalf("shell→python imports not probed: %+v", imp)
	}
	if statuses["shell_syntax"].Status != "passed" {
		t.Fatalf("shell_syntax: %+v", statuses["shell_syntax"])
	}
}

// Codex r1 #2b: local modules are FOLLOWED, not dropped — a broken
// third-party import two hops deep (train → helper → missing) is caught.
func TestLocalModuleGraphRecursion(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	writeF := func(name, content string) {
		if err := os.WriteFile(dir+"/"+name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeF("train.py", "import helper\n")
	writeF("helper.py", "import json\nimport nested_missing_module_zz\n")
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " train.py"}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, interp+" train.py", &report)
	for _, c := range results {
		if c.Name == "imports" {
			if c.Status != "failed" || !strings.Contains(c.Detail, "nested_missing_module_zz") {
				t.Fatalf("nested dependency missed: %+v", c)
			}
			if strings.Contains(c.Detail, `"helper"`) {
				t.Fatalf("local module wrongly probed: %+v", c)
			}
			return
		}
	}
	t.Fatal("no imports result")
}

// Codex r1 #3: the probe assembles the SAME environment run.sh gives the
// task — project environment and target env_setup are visible to every
// check (here: extra_run asserts on both).
func TestProbeEnvInjection(t *testing.T) {
	probePython(t)
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo hi"}
	proj.Preflight = &project.PreflightConfig{
		ExtraRun: `test "$FROM_PROJECT_ENV" = "pe" && test "$FROM_ENV_SETUP" = "es"`,
	}

	pf := DefaultPreflight()
	pf.Env = map[string]string{"FROM_PROJECT_ENV": "pe"}
	pf.EnvSetup = "export FROM_ENV_SETUP=es"
	var report Report
	results := pf.probeChecks(context.Background(), proj, "echo hi", &report)
	for _, c := range results {
		if c.Name == "extra_run" && c.Status != "passed" {
			t.Fatalf("env not injected into probe shell: %+v", c)
		}
	}

	// And the wandb credential check sees the injected env too — the
	// exact false negative from the review (run.sh exports the key,
	// preflight claimed it missing).
	wandbOn := true
	proj.Preflight = &project.PreflightConfig{Wandb: &wandbOn}
	pf.Env = map[string]string{"WANDB_API_KEY": "k-123"}
	results = pf.probeChecks(context.Background(), proj, "echo hi", &report)
	found := false
	for _, c := range results {
		if c.Name == "wandb" {
			found = true
			if c.Status != "passed" {
				t.Fatalf("wandb cred exported by the task env reported as: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("no wandb result")
	}
}

// Codex r1 #5: ProbeTimeout is a wall-clock bound — a stuck extra_run is
// killed with its whole process group, and the probe file is cleaned up
// from the Go side when the in-shell rm never got to run.
func TestProbeTimeoutIsWallClock(t *testing.T) {
	probePython(t)
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo hi"}
	proj.Preflight = &project.PreflightConfig{ExtraRun: "sleep 30"}

	pf := DefaultPreflight()
	pf.ProbeTimeout = 100 * time.Millisecond
	var report Report
	start := time.Now()
	results := pf.probeChecks(context.Background(), proj, "echo hi", &report)
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("100ms timeout took %s — not a wall-clock bound", elapsed)
	}
	// Timeout is never a failure: the stuck check reports skipped.
	for _, c := range results {
		if c.Status == "failed" {
			t.Fatalf("timeout must not fail: %+v", c)
		}
	}
	// No probe litter.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".runq_preflight_") {
			t.Fatalf("probe file left behind after timeout: %s", e.Name())
		}
	}
}

// Source discovery units.
func TestCollectEntrySources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/run.sh", []byte("python3 a.py\npython3 a.py\npython b.py --x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.py", "b.py"} {
		if err := os.WriteFile(dir+"/"+f, []byte("import os\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sources, sh, err := (Preflight{}).collectEntrySources("bash run.sh", dir)
	if err != nil || sh == "" || len(sources) != 2 {
		t.Fatalf("sh entry: sources=%d sh=%q err=%v", len(sources), sh, err)
	}
	sources, sh, err = (Preflight{}).collectEntrySources("python3 a.py", dir)
	if err != nil || sh != "" || len(sources) != 1 {
		t.Fatalf("py entry: sources=%d sh=%q err=%v", len(sources), sh, err)
	}
}
