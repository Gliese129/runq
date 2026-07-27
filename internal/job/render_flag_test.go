package job

import (
	"strings"
	"testing"
)

// store_true semantics: a truthy flag is a bare `--name`; a falsy flag
// is OMITTED — `--sample=false` fires the switch (feedback group 2).
func TestRenderWithFlags(t *testing.T) {
	flags := map[string]bool{"sample": true}

	got, err := RenderWithFlags("python eval.py {{args}}",
		TaskParams{"sample": true, "seed": 7}, nil, flags)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--sample") || strings.Contains(got, "--sample=") {
		t.Fatalf("truthy flag: %q", got)
	}
	if !strings.Contains(got, "--seed=7") {
		t.Fatalf("normal param broken: %q", got)
	}

	got, err = RenderWithFlags("python eval.py {{args}}",
		TaskParams{"sample": false, "seed": 7}, nil, flags)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "sample") {
		t.Fatalf("falsy flag must be omitted entirely: %q", got)
	}

	// String forms ride the same truthiness.
	got, _ = RenderWithFlags("x {{args}}", TaskParams{"sample": "false"}, nil, flags)
	if strings.Contains(got, "sample") {
		t.Fatalf(`"false" string: %q`, got)
	}
	got, _ = RenderWithFlags("x {{args}}", TaskParams{"sample": "true"}, nil, flags)
	if got != "x --sample" {
		t.Fatalf(`"true" string: %q`, got)
	}

	// Explicit {{name}} placeholders keep the raw value — naming a flag
	// in the template means the author wants the value itself.
	got, err = RenderWithFlags("x --s={{sample}}", TaskParams{"sample": false}, nil, flags)
	if err != nil || got != "x --s=false" {
		t.Fatalf("explicit placeholder: %q err=%v", got, err)
	}
}
