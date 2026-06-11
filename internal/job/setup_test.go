package job

import (
	"strings"
	"testing"
)

func setupCfg() *JobConfig {
	return &JobConfig{
		FixedParams: map[string]any{"model": "org/model-7b", "rev": 3},
		Sweep: []SweepBlock{{Method: "grid", Parameters: map[string]ParameterSpec{
			"lr": {Values: []any{0.1, 0.2}},
		}}},
	}
}

func TestRenderSetupFixedParams(t *testing.T) {
	out, err := RenderSetup("hf download {{model}} --rev {{rev}}", setupCfg())
	if err != nil {
		t.Fatal(err)
	}
	if out != "hf download org/model-7b --rev 3" {
		t.Errorf("got %q", out)
	}
}

func TestRenderSetupRejectsSweptParam(t *testing.T) {
	_, err := RenderSetup("echo {{lr}}", setupCfg())
	if err == nil {
		t.Fatal("swept param in setup must error")
	}
	if !strings.Contains(err.Error(), "swept") {
		t.Errorf("error should explain why: %v", err)
	}
}

func TestRenderSetupRejectsUnknown(t *testing.T) {
	if _, err := RenderSetup("echo {{nope}}", setupCfg()); err == nil {
		t.Fatal("unknown placeholder must error")
	}
}

func TestRenderSetupPlainPassthrough(t *testing.T) {
	out, err := RenderSetup("echo hello", setupCfg())
	if err != nil || out != "echo hello" {
		t.Fatalf("got %q, %v", out, err)
	}
}
