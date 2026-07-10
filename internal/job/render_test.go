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
	tests := []struct {
		name       string
		template   string
		params     TaskParams
		wantErrHas []string
	}{
		{
			name:       "one remaining param",
			template:   "python train.py --lr={{lr}}",
			params:     TaskParams{"lr": 0.001, "batch_size": 32},
			wantErrHas: []string{"batch_size"},
		},
		{
			name:       "all params remaining",
			template:   "python train.py",
			params:     TaskParams{"lr": 0.001, "batch_size": 32},
			wantErrHas: []string{"batch_size", "lr"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Render(tt.template, tt.params)
			if err == nil {
				t.Fatal("expected error for unconsumed params, got nil")
			}
			for _, s := range tt.wantErrHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error should mention %q, got: %v", s, err)
				}
			}
		})
	}
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

func TestRenderExcludingSchedulerParamsStayOutOfCommand(t *testing.T) {
	cmd, err := RenderExcluding(
		"python train.py --model={{model}} {{args}}",
		TaskParams{
			"model":     "llama",
			"lr":        0.001,
			"h_rt":      "4:00:00",
			"node_kind": "a100",
		},
		map[string]bool{"h_rt": true, "node_kind": true},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "python train.py --model=llama --lr=0.001"
	if cmd != expected {
		t.Errorf("got %q, want %q", cmd, expected)
	}
	if strings.Contains(cmd, "h_rt") || strings.Contains(cmd, "node_kind") {
		t.Errorf("scheduler-only params leaked into command: %q", cmd)
	}
}

func TestRenderDoesNotInferHostOrTargetShell(t *testing.T) {
	tests := []struct {
		name     string
		template string
		params   TaskParams
		want     string
	}{
		{
			name:     "posix target command stays posix shaped",
			template: "bash scripts/run.sh --data={{data_dir}} {{args}}",
			params: TaskParams{
				"data_dir": "/mnt/datasets/imagenet",
				"lr":       0.001,
			},
			want: "bash scripts/run.sh --data=/mnt/datasets/imagenet --lr=0.001",
		},
		{
			name:     "windows target command is not rewritten by host OS",
			template: "pwsh -File train.ps1 -Data {{data_dir}} {{args}}",
			params: TaskParams{
				"data_dir": `C:\runq\data`,
				"dry_run":  true,
			},
			want: `pwsh -File train.ps1 -Data C:\runq\data --dry_run=true`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render(tt.template, tt.params)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
