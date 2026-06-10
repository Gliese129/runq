package hpcconfig

import (
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
