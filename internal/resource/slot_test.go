package resource

import "testing"

func TestSlotAllocatorEnforcesMaxInflight(t *testing.T) {
	a := NewSlotAllocator(2)
	if err := a.Acquire("t1"); err != nil {
		t.Fatalf("acquire t1: %v", err)
	}
	if err := a.Acquire("t2"); err != nil {
		t.Fatalf("acquire t2: %v", err)
	}
	if err := a.Acquire("t3"); err == nil {
		t.Fatal("third acquire succeeded above max_inflight")
	}
	a.Release("t1")
	if err := a.Acquire("t3"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestSlotAllocatorRecoveryMayExceedReducedLimit(t *testing.T) {
	a := NewSlotAllocator(1)
	a.Reserve("restored-1")
	a.Reserve("restored-2")
	if got := a.FreeCount(); got != -1 {
		t.Fatalf("free count = %d, want -1 while restored overage drains", got)
	}
	if err := a.Acquire("new"); err == nil {
		t.Fatal("new acquire succeeded while restored tasks exceed the limit")
	}
	a.Release("restored-1")
	a.Release("restored-2")
	if err := a.Acquire("new"); err != nil {
		t.Fatalf("acquire after overage drained: %v", err)
	}
}

func TestSlotAllocatorUnlimitedAndIdempotent(t *testing.T) {
	a := NewSlotAllocator(0)
	if err := a.Acquire("same"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	before := a.FreeCount()
	if err := a.Acquire("same"); err != nil {
		t.Fatalf("idempotent acquire: %v", err)
	}
	if got := a.FreeCount(); got != before {
		t.Fatalf("idempotent acquire changed capacity: before=%d after=%d", before, got)
	}
}
