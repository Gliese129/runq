package job

import (
	"os"
	"reflect"
	"testing"
)

func writeArgparseScript(t *testing.T, content string) string {
	t.Helper()
	file := t.TempDir() + "/train.py"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatalf("write argparse fixture: %v", err)
	}
	return file
}

func TestScanArgparseCommonForms(t *testing.T) {
	file := writeArgparseScript(t, `
import argparse

parser = argparse.ArgumentParser()
parser.add_argument("--lr", type=float, default=0.001)
parser.add_argument("--batch-size", type=int, default=32)
parser.add_argument("--optimizer", default="adam")
parser.add_argument("--resume", action="store_true")
parser.add_argument("dataset")
`)

	args, err := ScanArgparse(file)
	if err != nil {
		t.Fatalf("ScanArgparse: %v", err)
	}

	expected := []ArgInfo{
		{"lr", "float", "0.001", ""},
		{"batch-size", "int", "32", ""},
		{"optimizer", "", "adam", ""},
		{"resume", "bool", "false", "flag"},
	}
	if !reflect.DeepEqual(args, expected) {
		t.Fatalf("args = %+v, want %+v", args, expected)
	}
}

func TestScanArgparseContractCases(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		want    []ArgInfo
		wantErr bool
	}{
		{
			name: "multiline add_argument keeps order",
			script: `
import argparse
p = argparse.ArgumentParser()
p.add_argument(
    "--model",
    type=str,
    default="llama",
)
p.add_argument("--epochs", type=int, default=3)
`,
			want: []ArgInfo{
				{"model", "str", "llama", ""},
				{"epochs", "int", "3", ""},
			},
		},
		{
			name: "single quoted flags and bool action",
			script: `
import argparse
p = argparse.ArgumentParser()
p.add_argument('--dry-run', action='store_true')
`,
			want: []ArgInfo{{"dry-run", "bool", "false", "flag"}},
		},
		{
			name: "short aliases do not replace long canonical flag",
			script: `
import argparse
p = argparse.ArgumentParser()
p.add_argument("-b", "--batch-size", type=int, default=16)
`,
			want: []ArgInfo{{"batch-size", "int", "16", ""}},
		},
		{
			name: "positional arguments are skipped",
			script: `
import argparse
p = argparse.ArgumentParser()
p.add_argument("dataset")
p.add_argument("--seed", type=int, default=7)
`,
			want: []ArgInfo{{"seed", "int", "7", ""}},
		},
		{
			name:    "missing file returns error",
			script:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := "/definitely/missing/train.py"
			if !tt.wantErr {
				file = writeArgparseScript(t, tt.script)
			}
			got, err := ScanArgparse(file)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ScanArgparse: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScanArgparseIsBestEffortTextScan(t *testing.T) {
	file := writeArgparseScript(t, `
import argparse
p = argparse.ArgumentParser()

if False:
    p.add_argument("--inactive", default="still-seen")

def register(parser):
    parser.add_argument("--inside-helper", default="also-seen")
`)

	args, err := ScanArgparse(file)
	if err != nil {
		t.Fatalf("ScanArgparse: %v", err)
	}
	want := []ArgInfo{
		{"inactive", "", "still-seen", ""},
		{"inside-helper", "", "also-seen", ""},
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
}

// Python `default=None` means NO default — the literal string "None"
// must never leak into project.yaml / rendered commands (feedback
// group 2: `-max_new_tokens None` rejected by argparse after queueing).
func TestScanArgparseNoneDefault(t *testing.T) {
	file := writeArgparseScript(t, `
import argparse
p = argparse.ArgumentParser()
p.add_argument("--max_new_tokens", type=int, default=None)
p.add_argument("--name", type=str, default="None")
`)
	args, err := ScanArgparse(file)
	if err != nil {
		t.Fatal(err)
	}
	// Unquoted None → no default; the QUOTED string "None" is
	// indistinguishable after quote-stripping and is normalized too —
	// a script that truly wants the literal string "None" as a default
	// is pathological enough to not design for.
	if args[0].Default != "" {
		t.Fatalf("default=None captured as %q, want empty", args[0].Default)
	}
	if args[1].Default != "" {
		t.Fatalf(`default="None" captured as %q, want empty`, args[1].Default)
	}
}
