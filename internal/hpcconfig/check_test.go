package hpcconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		SubmitTemplate: "sbatch --gres=gpu:{{gpus}} {{run_sh}}",
		SubmitIDRegex:  `Submitted batch job ([0-9]+)`,
		KillTemplate:   "scancel {{ext_id}}",
	}
}

func findResult(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no check result named %q", name)
	return CheckResult{}
}

func TestCheckValidConfig(t *testing.T) {
	results := validConfig().Check()

	sub := findResult(t, results, "submit_template")
	if sub.Status != "ok" {
		t.Fatalf("submit_template: want ok, got %s (%s)", sub.Status, sub.Detail)
	}
	if !strings.Contains(sub.Detail, "run.sh") {
		t.Errorf("submit preview should contain rendered run_sh, got %q", sub.Detail)
	}

	if r := findResult(t, results, "submit_id_regex"); r.Status != "ok" {
		t.Errorf("regex: want ok, got %s", r.Status)
	}
	if r := findResult(t, results, "status_template"); r.Status != "skip" {
		t.Errorf("unset status_template: want skip, got %s", r.Status)
	}
	if r := findResult(t, results, "status_parser"); r.Status != "skip" {
		t.Errorf("unset status_parser: want skip, got %s", r.Status)
	}
}

func TestCheckParamNamespace(t *testing.T) {
	cfg := validConfig()
	cfg.SubmitTemplate = `qsub -l {{param.node_kind}}=1 -l h_rt={{param.h_rt}} -N {{param.lang}}_{{param.task}} {{run_sh}}`
	r := findResult(t, cfg.Check(), "submit_template")
	if r.Status != "ok" {
		t.Fatalf("param.* refs should check ok, got %s (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "<h_rt>") {
		t.Errorf("preview should show synthesized param sample, got %q", r.Detail)
	}

	// param.* is submit-only: kill_template referencing it must fail.
	cfg2 := validConfig()
	cfg2.KillTemplate = "scancel {{param.h_rt}}"
	if r := findResult(t, cfg2.Check(), "kill_template"); r.Status != "fail" {
		t.Errorf("param.* in kill_template should fail, got %s", r.Status)
	}
}

func TestCheckUnknownPlaceholder(t *testing.T) {
	cfg := validConfig()
	cfg.SubmitTemplate = "sbatch {{run_sh}} --id={{taskid}}" // typo: taskid
	r := findResult(t, cfg.Check(), "submit_template")
	if r.Status != "fail" {
		t.Fatalf("typo placeholder: want fail, got %s", r.Status)
	}
	if !strings.Contains(r.Detail, "{{task_id}}") {
		t.Errorf("failure should list valid placeholders, got %q", r.Detail)
	}
}

func TestCheckRegexCaptureGroups(t *testing.T) {
	cfg := validConfig()
	cfg.SubmitIDRegex = `job ([0-9]+) on ([a-z]+)` // 2 groups
	r := findResult(t, cfg.Check(), "submit_id_regex")
	if r.Status != "fail" {
		t.Fatalf("2 capture groups: want fail, got %s", r.Status)
	}
}

func TestCheckParserStages(t *testing.T) {
	cfg := validConfig()
	cfg.StatusTemplate = "sacct -j {{ext_id}}"
	cfg.StatusParser = []string{"grep -o 'COMPLETED'", "awk '{print tolower($0)}' # {{ext_id}}"}
	results := cfg.Check()
	if r := findResult(t, results, "status_template"); r.Status != "ok" {
		t.Errorf("status_template: want ok, got %s (%s)", r.Status, r.Detail)
	}
	if r := findResult(t, results, "status_parser[1]"); r.Status != "ok" {
		t.Errorf("parser stage: want ok, got %s (%s)", r.Status, r.Detail)
	}
}

// Every shipped preset must parse and pass its own check (presets are the
// first thing a new cluster user sees — they must never be born broken).
func TestPresetsParseAndCheck(t *testing.T) {
	for _, name := range Presets() {
		cfg, err := Preset(name)
		if err != nil {
			t.Fatalf("preset %q: %v", name, err)
		}
		for _, r := range cfg.Check() {
			if r.Status == "fail" {
				t.Errorf("preset %q: %s fails check: %s", name, r.Name, r.Detail)
			}
		}
	}
}

// Regression: the GUI reads/writes this struct as JSON — keys must be
// snake_case (missing json tags once made presets fill empty forms).
func TestConfigJSONUsesSnakeCase(t *testing.T) {
	cfg, err := Preset("tsubame")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, key := range []string{"submit_template", "submit_id_regex", "status_template", "status_parser", "kill_template"} {
		if !strings.Contains(s, `"`+key+`"`) {
			t.Errorf("JSON missing snake_case key %q: %s", key, s)
		}
	}
}
