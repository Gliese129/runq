package resource

import (
	"strings"
	"testing"
)

const fourGPU = `0, NVIDIA A100-SXM4-80GB, 79000, 1000, 5
1, NVIDIA A100-SXM4-80GB, 80000, 0, 0
2, NVIDIA A100-SXM4-80GB, 40000, 40000, 95
3, NVIDIA A100-SXM4-80GB, 80000, 0, 0`

func TestParseFourGPU(t *testing.T) {
	infos, err := Parse(fourGPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 4 {
		t.Fatalf("expected 4 GPUs, got %d", len(infos))
	}
	if infos[0].Index != 0 || infos[0].MemFree != 79000 {
		t.Errorf("GPU 0 mismatch: %+v", infos[0])
	}
}

func TestParseEmpty(t *testing.T) {
	infos, err := Parse("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("expected 0 GPUs, got %d", len(infos))
	}
}

func TestParseBadFieldCount(t *testing.T) {
	_, err := Parse("0, NVIDIA A100, 79000")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "expected 5 fields") {
		t.Errorf("wrong error: %v", err)
	}
}
