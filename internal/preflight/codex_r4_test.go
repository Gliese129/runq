package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
)

// The r4 doctrine (user ruling): paths resolve from working_dir, runq
// does not follow `cd`. An entry we cannot locate is a declared
// BOUNDARY — skipped + guidance — never a failure. False positives are
// bugs; false negatives here have a 30-second user-side fix
// (preflight.hf / preflight.extra_run).

// The exact r4 repro: `cd eval && python3 eval.py` inside the shell
// entry. eval.py does NOT exist at working_dir — a legal task that must
// NOT be blocked.
func TestR4CdInShellEntryNeverFails(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	writeF := func(name, content string) {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeF("run.sh", "#!/bin/sh\ncd eval && "+interp+" eval.py\n")
	writeF("eval/eval.py", "import json\n")
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "bash run.sh"}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, "bash run.sh", &report)
	for _, c := range results {
		if c.Status == "failed" {
			t.Fatalf("legal cd-style task blocked (false positive): %+v", c)
		}
		if c.Name == "imports" {
			if c.Status != "skipped" || !strings.Contains(c.Detail, "working_dir") || !strings.Contains(c.Detail, "cd") {
				t.Fatalf("entry miss must skip with the doctrine guidance: %+v", c)
			}
		}
	}
}

// Direct command with a missing script: same doctrine, same skip.
func TestR4MissingEntryScriptSkips(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " nope.py"}
	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, interp+" nope.py", &report)
	for _, c := range results {
		if c.Name == "imports" {
			if c.Status != "skipped" || !strings.Contains(c.Detail, "working_dir") {
				t.Fatalf("missing entry: %+v", c)
			}
			return
		}
	}
	t.Fatal("no imports result")
}

// A slow probe upload cannot hold preflight past its deadline (the last
// unbounded transport call — Read was r3, Write is r4).
func TestR4SlowWriteFileDeadline(t *testing.T) {
	dir := t.TempDir()
	slow := slowWriteFS{FS: rfs.NewLocalFS(), delay: 250 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := writeFileCtx(ctx, slow, filepath.Join(dir, "probe.py"), []byte("x"), 0o644, nil)
	if err == nil {
		t.Fatal("deadline not enforced")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("writeFileCtx held past deadline: %s", elapsed)
	}
}

type slowWriteFS struct {
	rfs.FS
	delay time.Duration
}

func (s slowWriteFS) WriteFile(p string, data []byte, perm os.FileMode) error {
	time.Sleep(s.delay)
	return s.FS.WriteFile(p, data, perm)
}

// `from json import missing_name` stays NON-blocking: the name may be
// an attribute — only import-statement MODULE paths are certain.
func TestR4FromImportNameStaysLenient(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("from json import there_is_no_such_name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " train.py"}
	pf := DefaultPreflight()
	var report Report
	for _, c := range pf.probeChecks(context.Background(), proj, interp+" train.py", &report) {
		if c.Name == "imports" && c.Status == "failed" {
			t.Fatalf("from-import name wrongly hard-failed: %+v", c)
		}
	}
}
