package project

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression for the RQ-69 review finding: `gpus_per_task: 0` in
// project.yaml reached the dashboard as `"defaults": {}` because the JSON
// tags used omitempty — an int omitempty cannot distinguish "unset" from
// a legal explicit 0 (gpus_per_task 0 = CPU-only, max_retry 0 = no
// retries; unlimited is an explicit -1). The wire must always carry both
// ints explicitly.
func TestDefaultsExplicitZeroSurvivesJSONRoundTrip(t *testing.T) {
	var cfg Config
	yamlSrc := strings.Join([]string{
		"project_name: p1",
		"working_dir: /w",
		"command_template: python train.py {{args}}",
		"defaults:",
		"  gpus_per_task: 0",
		"  max_retry: 0",
	}, "\n")
	if err := yaml.Unmarshal([]byte(yamlSrc), &cfg); err != nil {
		t.Fatalf("yaml: %v", err)
	}

	wire, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"gpus_per_task":0`, `"max_retry":0`} {
		if !strings.Contains(string(wire), want) {
			t.Errorf("wire JSON dropped explicit zero — missing %s in %s", want, wire)
		}
	}

	// The dashboard PUTs the same shape back — it must decode losslessly.
	var back Config
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Defaults.GPUsPerTask != 0 || back.Defaults.MaxRetry != 0 {
		t.Errorf("round trip mutated defaults: %+v", back.Defaults)
	}
}

// The max_retry contract admits exactly one negative: -1 (unlimited).
// Runtime stays tolerant of legacy negatives, but every intake boundary
// (project save handlers, submit planning) must reject them.
func TestValidateRetryBounds(t *testing.T) {
	for _, ok := range []int{-1, 0, 1, 100} {
		if err := ValidateRetryBounds(ok); err != nil {
			t.Errorf("ValidateRetryBounds(%d) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []int{-2, -7, -100} {
		if err := ValidateRetryBounds(bad); err == nil {
			t.Errorf("ValidateRetryBounds(%d) = nil, want error", bad)
		}
	}
}

// An OMITTED yaml key decodes to the same 0 the scheduler uses (unset ==
// 0 is the established semantic; submitplan consumes the zero value
// directly). The wire now reports that 0 explicitly instead of hiding it.
func TestDefaultsOmittedYAMLKeyIsExplicitZeroOnWire(t *testing.T) {
	var cfg Config
	yamlSrc := "project_name: p1\nworking_dir: /w\ncommand_template: x\n"
	if err := yaml.Unmarshal([]byte(yamlSrc), &cfg); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	wire, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"gpus_per_task":0`) {
		t.Errorf("unset defaults must still serialize explicitly: %s", wire)
	}
}
