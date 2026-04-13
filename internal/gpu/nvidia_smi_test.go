package gpu

import (
	"strings"
	"testing"
)

const fourGPU = `0, NVIDIA A100-SXM4-80GB, 79000, 1000, 5
1, NVIDIA A100-SXM4-80GB, 80000, 0, 0
2, NVIDIA A100-SXM4-80GB, 40000, 40000, 95
3, NVIDIA A100-SXM4-80GB, 80000, 0, 0`

const singleGPU = `0, NVIDIA RTX 4090, 23000, 1000, 12`

const badFieldCount = `0, NVIDIA A100, 79000`

const badNumber = `0, NVIDIA A100, notanumber, 1000, 5`

func TestParseFourGPU(t *testing.T) {
	infos, err := Parse(fourGPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 4 {
		t.Fatalf("expected 4 GPUs, got %d", len(infos))
	}

	// Spot-check first and third
	if infos[0].Index != 0 || infos[0].Name != "NVIDIA A100-SXM4-80GB" ||
		infos[0].MemFree != 79000 || infos[0].MemUsed != 1000 || infos[0].UtilPct != 5 {
		t.Errorf("GPU 0 mismatch: %+v", infos[0])
	}
	if infos[2].Index != 2 || infos[2].MemFree != 40000 || infos[2].MemUsed != 40000 || infos[2].UtilPct != 95 {
		t.Errorf("GPU 2 mismatch: %+v", infos[2])
	}
}

func TestParseSingleGPU(t *testing.T) {
	infos, err := Parse(singleGPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(infos))
	}
	if infos[0].Name != "NVIDIA RTX 4090" || infos[0].MemFree != 23000 || infos[0].UtilPct != 12 {
		t.Errorf("GPU 0 mismatch: %+v", infos[0])
	}
}

func TestParseEmpty(t *testing.T) {
	for _, input := range []string{"", "  ", "  \n  ", "\n\n"} {
		infos, err := Parse(input)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", input, err)
		}
		if len(infos) != 0 {
			t.Errorf("Parse(%q) expected 0 GPUs, got %d", input, len(infos))
		}
	}
}

func TestParseBadFieldCount(t *testing.T) {
	_, err := Parse(badFieldCount)
	if err == nil {
		t.Fatal("expected error for bad field count, got nil")
	}
	if !strings.Contains(err.Error(), "expected 5 fields") {
		t.Errorf("error should mention field count, got: %v", err)
	}
}

func TestParseBadNumber(t *testing.T) {
	_, err := Parse(badNumber)
	if err == nil {
		t.Fatal("expected error for bad number, got nil")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error should mention line number, got: %v", err)
	}
}

func TestParseTrailingNewline(t *testing.T) {
	infos, err := Parse(fourGPU + "\n\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 4 {
		t.Errorf("expected 4 GPUs, got %d", len(infos))
	}
}
