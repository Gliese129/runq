package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A freshly written project.yaml carries the commented preflight
// scaffold; the block must PARSE back to the same defaults an absent
// block implies — the scaffold makes defaults visible, never different.
func TestWriteYAMLScaffoldsPreflight(t *testing.T) {
	dir := t.TempDir()
	c := &Config{
		ProjectName: "p", WorkingDir: dir, CmdTemplate: "python train.py {{args}}",
		Params: []ParamDef{{Name: "model_name", Type: "str"}, {Name: "seed", Type: "int"}},
	}
	if err := c.WriteYAML(nil); err != nil {
		t.Fatal(err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(buf)
	if !strings.Contains(text, "preflight:") || !strings.Contains(text, "enabled: true") {
		t.Fatalf("scaffold missing:\n%s", text)
	}
	// model-ish param suggested, quoted (bare {{ is invalid YAML).
	if !strings.Contains(text, `- "{{param.model_name}}"`) {
		t.Fatalf("param suggestion missing:\n%s", text)
	}
	if strings.Contains(text, "{{param.seed}}") {
		t.Fatalf("non-model param wrongly suggested:\n%s", text)
	}

	var back Config
	if err := yaml.Unmarshal(buf, &back); err != nil {
		t.Fatalf("scaffolded yaml does not parse: %v\n%s", err, text)
	}
	pf := back.Preflight
	if pf == nil {
		t.Fatal("scaffold block not parsed into Preflight")
	}
	if !pf.EnabledOrDefault() || !pf.ImportsOrDefault() || pf.WandbOrDefault() || pf.ExtraRunOrEmpty() != "" {
		t.Fatalf("scaffold semantics differ from defaults: %+v", pf)
	}
	if len(pf.HF) != 1 || pf.HF[0] != "{{param.model_name}}" {
		t.Fatalf("hf suggestion parsed wrong: %+v", pf.HF)
	}

	// A config that already HAS a preflight block gets no scaffold text.
	dir2 := t.TempDir()
	on := true
	c2 := &Config{ProjectName: "p", WorkingDir: dir2, CmdTemplate: "x",
		Preflight: &PreflightConfig{Enabled: &on, HF: []string{"org/m"}}}
	if err := c2.WriteYAML(nil); err != nil {
		t.Fatal(err)
	}
	buf2, _ := os.ReadFile(filepath.Join(dir2, "project.yaml"))
	if strings.Count(string(buf2), "preflight:") != 1 {
		t.Fatalf("double preflight block:\n%s", buf2)
	}
	if !strings.Contains(string(buf2), "org/m") {
		t.Fatalf("declared block lost:\n%s", buf2)
	}
}
