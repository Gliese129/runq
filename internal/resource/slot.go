package resource

import (
	"fmt"
	"sync"
)

// unlimitedSlots is the FreeCount reported by an uncapped
// SlotAllocator. Large enough that the scheduler never sees backpressure.
const unlimitedSlots = 1 << 20

// SlotAllocator limits tasks in flight on an external scheduler. Slots have
// no identity; each task occupies exactly one. total <= 0 means unlimited.
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

// Acquire occupies one slot for taskID. Re-acquiring an already-held task is
// idempotent so recovery and dispatch cannot double-count it.
func (a *SlotAllocator) Acquire(taskID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.used[taskID]; ok {
		return nil
	}
	if a.total > 0 && len(a.used) >= a.total {
		return fmt.Errorf("no free submission slots (%d in flight, max %d)", len(a.used), a.total)
	}
	a.used[taskID] = struct{}{}
	return nil
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

// Reserve re-occupies a slot for an in-flight task on daemon restart. Slot
// counts are allowed to exceed a newly lowered max_inflight; the overage
// drains as restored tasks finish.
func (a *SlotAllocator) Reserve(taskID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[taskID] = struct{}{}
}
