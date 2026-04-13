package job

import (
	"strings"
	"testing"
)

func TestRenderArgs(t *testing.T) {
	cmd, err := Render("python train.py {{args}}", TaskParams{
		"lr":         0.001,
		"batch_size": 32,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Keys sorted: batch_size before lr
	expected := "python train.py --batch_size=32 --lr=0.001"
	if cmd != expected {
		t.Errorf("got %q, want %q", cmd, expected)
	}
}

func TestRenderNamed(t *testing.T) {
	cmd, err := Render(
		"python train.py --learning-rate={{lr}} --bs={{batch_size}}",
		TaskParams{"lr": 0.001, "batch_size": 32},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "python train.py --learning-rate=0.001 --bs=32"
	if cmd != expected {
		t.Errorf("got %q, want %q", cmd, expected)
	}
}

func TestRenderMixed(t *testing.T) {
	cmd, err := Render(
		"python train.py --lr={{lr}} {{args}}",
		TaskParams{"lr": 0.001, "batch_size": 32, "optimizer": "adam"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// lr consumed by named, remaining sorted: batch_size, optimizer
	expected := "python train.py --lr=0.001 --batch_size=32 --optimizer=adam"
	if cmd != expected {
		t.Errorf("got %q, want %q", cmd, expected)
	}
}

func TestRenderMissingParam(t *testing.T) {
	_, err := Render(
		"python train.py --lr={{lr}} --wd={{weight_decay}}",
		TaskParams{"lr": 0.001},
	)
	if err == nil {
		t.Fatal("expected error for missing param, got nil")
	}
	if !strings.Contains(err.Error(), "weight_decay") {
		t.Errorf("error should mention 'weight_decay', got: %v", err)
	}
	t.Logf("got expected error: %v", err)
}

func TestRenderUnconsumedParams(t *testing.T) {
	_, err := Render(
		"python train.py --lr={{lr}}",
		TaskParams{"lr": 0.001, "batch_size": 32},
	)
	if err == nil {
		t.Fatal("expected error for unconsumed params, got nil")
	}
	if !strings.Contains(err.Error(), "batch_size") {
		t.Errorf("error should mention 'batch_size', got: %v", err)
	}
	t.Logf("got expected error: %v", err)
}

func TestRenderNoPlaceholder(t *testing.T) {
	cmd, err := Render("python train.py", TaskParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "python train.py" {
		t.Errorf("got %q, want %q", cmd, "python train.py")
	}
}

func TestRenderEmptyArgs(t *testing.T) {
	cmd, err := Render(
		"python train.py --lr={{lr}} {{args}}",
		TaskParams{"lr": 0.001},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All params consumed by named, {{args}} becomes empty, trailing space trimmed
	expected := "python train.py --lr=0.001"
	if cmd != expected {
		t.Errorf("got %q, want %q", cmd, expected)
	}
}

func TestRenderStringValue(t *testing.T) {
	cmd, err := Render("python train.py {{args}}", TaskParams{
		"optimizer": "adam",
		"lr":        0.001,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "python train.py --lr=0.001 --optimizer=adam"
	if cmd != expected {
		t.Errorf("got %q, want %q", cmd, expected)
	}
}
