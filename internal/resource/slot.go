package resource

import (
	"fmt"
	"sync"
)

// unlimitedSlots is the FreeCount/TotalCount reported by an uncapped
// SlotAllocator. Large enough that the scheduler never sees backpressure.
const unlimitedSlots = 1 << 20

// SlotAllocator is an Allocator over abstract submission slots instead of
// physical GPUs. It backs remote (unsupervised) scheduler lanes, where the
// resource being rationed is "tasks in flight on the external scheduler"
// (max_inflight), not local hardware.
//
// Slots have no identity, so Allocate returns an empty index slice and
// Status reports nothing (remote targets have no GPU map). total <= 0 means
// unlimited.
type SlotAllocator struct {
	total int
	mu    sync.Mutex
	used  map[string]struct{}
}

// NewSlotAllocator creates a SlotAllocator with the given capacity.
// total <= 0 disables the cap.
func NewSlotAllocator(total int) *SlotAllocator {
	return &SlotAllocator{total: total, used: make(map[string]struct{})}
}

// Allocate occupies one slot for taskID. n is ignored — a remote task always
// occupies exactly one submission slot; its cluster-side GPU request lives in
// the submit template, not here.
func (a *SlotAllocator) Allocate(n int, taskID string) ([]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.total > 0 && len(a.used) >= a.total {
		return nil, fmt.Errorf("no free submission slots (%d in flight, max %d)", len(a.used), a.total)
	}
	a.used[taskID] = struct{}{}
	return []int{}, nil
}

// Release frees taskID's slot. No-op if not held.
func (a *SlotAllocator) Release(taskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, taskID)
}

// FreeCount returns remaining slots (a large constant when uncapped).
func (a *SlotAllocator) FreeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.total <= 0 {
		return unlimitedSlots
	}
	return a.total - len(a.used)
}

// TotalCount returns the slot capacity (a large constant when uncapped).
func (a *SlotAllocator) TotalCount() int {
	if a.total <= 0 {
		return unlimitedSlots
	}
	return a.total
}

// Status returns an empty GPU map — remote targets expose no local GPUs.
func (a *SlotAllocator) Status() []GPUState { return []GPUState{} }

// Reserve re-occupies a slot for an in-flight task on daemon restart. Slot
// indices are meaningless; only occupancy counts. Never fails: restored
// in-flight tasks must be tracked even if config lowered max_inflight below
// the restored count (the overage drains as tasks finish).
func (a *SlotAllocator) Reserve(indices []int, taskID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[taskID] = struct{}{}
	return nil
}

// compile-time interface check
var _ Allocator = (*SlotAllocator)(nil)
