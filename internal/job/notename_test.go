package job

import (
	"testing"
	"time"
)

func noteCfg(note string) *JobConfig {
	return &JobConfig{
		Note: note,
		FixedParams: map[string]any{
			"epochs": 100,
			"model":  "resnet50",
		},
		Sweep: []SweepBlock{
			{Method: "grid", Parameters: map[string]ParameterSpec{
				"lr": {Values: []any{0.001, 0.01, 0.1}},
				"bs": {Values: []any{32, 64}},
			}},
		},
	}
}

func nc(existing ...string) NoteContext {
	return NoteContext{
		Project:       "pytrain",
		User:          "alice",
		Now:           time.Date(2026, 6, 10, 14, 30, 0, 0, time.UTC),
		ExistingNotes: existing,
	}
}

func render(t *testing.T, note string, ctx NoteContext) string {
	t.Helper()
	out, err := RenderNote(noteCfg(note), ctx)
	if err != nil {
		t.Fatalf("RenderNote(%q): %v", note, err)
	}
	return out
}

func TestPlainNotePassthrough(t *testing.T) {
	if got := render(t, "no placeholders here", nc()); got != "no placeholders here" {
		t.Errorf("got %q", got)
	}
}

func TestStablePlaceholders(t *testing.T) {
	got := render(t, "{{project}}-{{user}}-{{model}}-e{{epochs}}", nc())
	if got != "pytrain-alice-resnet50-e100" {
		t.Errorf("got %q", got)
	}
}

func TestSweptParamAndSweep(t *testing.T) {
	got := render(t, "{{lr}}_{{sweep}}", nc())
	if got != "lr(3)_bs(2)xlr(3)" {
		t.Errorf("got %q", got)
	}
}

func TestVolatiles(t *testing.T) {
	got := render(t, "{{date}}-{{time}}", nc())
	if got != "20260610-1430" {
		t.Errorf("got %q", got)
	}
}

func TestVersionFirstRunSwallowsSeparator(t *testing.T) {
	if got := render(t, "distill-{{version}}", nc()); got != "distill" {
		t.Errorf("v1: got %q", got)
	}
}

func TestVersionIncrements(t *testing.T) {
	if got := render(t, "distill-{{version}}", nc("distill")); got != "distill-v2" {
		t.Errorf("v2: got %q", got)
	}
	if got := render(t, "distill-{{version}}", nc("distill", "distill-v2", "distill-v7")); got != "distill-v8" {
		t.Errorf("v8: got %q", got)
	}
}

func TestVersionIgnoresUnrelatedNotes(t *testing.T) {
	got := render(t, "distill-{{version}}", nc("other-job", "distillery", "distill-v2-extra"))
	if got != "distill" {
		t.Errorf("unrelated notes should not raise the version: got %q", got)
	}
}

func TestVersionFamilyIgnoresTimestampDiff(t *testing.T) {
	// Yesterday's run was nerf-20260609; today's must still be its family.
	got := render(t, "nerf-{{date}}-{{version}}", nc("nerf-20260609"))
	if got != "nerf-20260610-v2" {
		t.Errorf("volatile wildcard: got %q", got)
	}
}

func TestVersionWithoutSeparator(t *testing.T) {
	if got := render(t, "run{{version}}", nc()); got != "run" {
		t.Errorf("v1 no-sep: got %q", got)
	}
	if got := render(t, "run{{version}}", nc("run")); got != "runv2" {
		t.Errorf("v2 no-sep: got %q", got)
	}
}

func TestUnknownPlaceholderFails(t *testing.T) {
	if _, err := RenderNote(noteCfg("{{nope}}"), nc()); err == nil {
		t.Fatal("unknown placeholder should error")
	}
}
