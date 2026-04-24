package resource

import "testing"

func makePool(n int) *GPUPool {
	infos := make([]Info, n)
	for i := 0; i < n; i++ {
		infos[i] = Info{Index: i, Name: "Test GPU", MemFree: 80000, UtilPct: 0}
	}
	return NewGPUPool(infos)
}

func TestAllocateBasic(t *testing.T) {
	pool := makePool(4)
	gpus, err := pool.Allocate(2, "task-1")
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(gpus))
	}
	if gpus[0] != 0 || gpus[1] != 1 {
		t.Errorf("expected [0,1], got %v", gpus)
	}
	if pool.FreeCount() != 2 {
		t.Errorf("expected 2 free, got %d", pool.FreeCount())
	}
}

func TestAllocateNotEnough(t *testing.T) {
	pool := makePool(4)
	if _, err := pool.Allocate(3, "task-1"); err != nil {
		t.Fatalf("first Allocate failed: %v", err)
	}
	_, err := pool.Allocate(2, "task-2")
	if err == nil {
		t.Fatal("expected error when not enough GPUs, got nil")
	}
	if pool.FreeCount() != 1 {
		t.Errorf("expected 1 free after failed Allocate, got %d", pool.FreeCount())
	}
}

func TestAllocateAll(t *testing.T) {
	pool := makePool(2)
	gpus, err := pool.Allocate(2, "task-1")
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2, got %d", len(gpus))
	}
	if pool.FreeCount() != 0 {
		t.Errorf("expected 0 free, got %d", pool.FreeCount())
	}
}

func TestRelease(t *testing.T) {
	pool := makePool(4)
	pool.Allocate(2, "task-1")
	pool.Allocate(2, "task-2")
	pool.Release("task-1")
	if pool.FreeCount() != 2 {
		t.Errorf("expected 2 free, got %d", pool.FreeCount())
	}
	gpus, _ := pool.Allocate(2, "task-3")
	if gpus[0] != 0 || gpus[1] != 1 {
		t.Errorf("expected freed GPUs [0,1], got %v", gpus)
	}
}

func TestReleaseIdempotent(t *testing.T) {
	pool := makePool(4)
	pool.Release("nonexistent")
	if pool.FreeCount() != 4 {
		t.Errorf("expected 4 free, got %d", pool.FreeCount())
	}
}

func TestStatus(t *testing.T) {
	pool := makePool(4)
	pool.Allocate(1, "task-1")
	status := pool.Status()
	if len(status) != 4 {
		t.Fatalf("expected 4 GPUs, got %d", len(status))
	}
	if status[0].TaskID != "task-1" {
		t.Errorf("GPU 0 should be task-1, got %q", status[0].TaskID)
	}
	// Mutating returned slice should not affect pool.
	status[0].TaskID = "hacked"
	if pool.Status()[0].TaskID != "task-1" {
		t.Error("Status returned internal pointer, not a copy")
	}
}

func TestFreeCount(t *testing.T) {
	pool := makePool(8)
	if pool.FreeCount() != 8 {
		t.Fatalf("expected 8, got %d", pool.FreeCount())
	}
	pool.Allocate(3, "a")
	pool.Allocate(5, "b")
	pool.Release("a")
	if pool.FreeCount() != 3 {
		t.Errorf("expected 3, got %d", pool.FreeCount())
	}
}

func TestAllocateZero(t *testing.T) {
	pool := makePool(4)
	gpus, err := pool.Allocate(0, "task-0")
	if err != nil {
		t.Fatalf("Allocate(0) failed: %v", err)
	}
	if len(gpus) != 0 {
		t.Errorf("expected 0 GPUs, got %d", len(gpus))
	}
}

func TestMockAllocator(t *testing.T) {
	mock := NewMockAllocator(4)
	gpus, err := mock.Allocate(2, "task-1")
	if err != nil {
		t.Fatalf("MockAllocator.Allocate failed: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(gpus))
	}
	if mock.FreeCount() != 2 {
		t.Errorf("expected 2 free, got %d", mock.FreeCount())
	}
	mock.Release("task-1")
	if mock.FreeCount() != 4 {
		t.Errorf("expected 4 free after release, got %d", mock.FreeCount())
	}
}
