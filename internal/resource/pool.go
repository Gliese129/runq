package resource

import (
	"fmt"
	"slices"
	"sort"
	"sync"
)

// GPUState tracks the status of a single GPU.
type GPUState struct {
	Index   int    `json:"index"`
	TaskID  string `json:"task_id,omitempty"` // empty if free
	MemFree int    `json:"mem_free"`          // MB
	UtilPct int    `json:"util_pct"`          // %
}

// GPUPool manages GPU allocation on the local machine.
// Implements the Allocator interface. Thread-safe.
type GPUPool struct {
	mu   sync.Mutex
	gpus map[int]*GPUState
}

// NewGPUPool initializes a GPUPool from detected GPU info.
func NewGPUPool(infos []Info) *GPUPool {
	gpus := make(map[int]*GPUState, len(infos))
	for _, info := range infos {
		gpus[info.Index] = &GPUState{
			Index:   info.Index,
			MemFree: info.MemFree,
			UtilPct: info.UtilPct,
		}
	}
	return &GPUPool{gpus: gpus}
}

// Allocate assigns n free GPUs to taskID. Returns sorted indices.
// Atomic: all-or-nothing — on error, no state is modified.
func (p *GPUPool) Allocate(n int, taskID string) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	free := make([]int, 0, len(p.gpus))
	for _, idx := range p.sortedIndices() {
		if p.gpus[idx].TaskID == "" {
			free = append(free, idx)
		}
	}

	if len(free) < n {
		return nil, fmt.Errorf("not enough free GPUs: requested %d, available %d", n, len(free))
	}

	allocated := free[:n]
	for _, idx := range allocated {
		p.gpus[idx].TaskID = taskID
	}
	return allocated, nil
}

// Release frees all GPUs assigned to taskID. Idempotent.
func (p *GPUPool) Release(taskID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, g := range p.gpus {
		if g.TaskID == taskID {
			g.TaskID = ""
		}
	}
}

// Status returns a snapshot of all GPU states, sorted by index.
func (p *GPUPool) Status() []GPUState {
	p.mu.Lock()
	defer p.mu.Unlock()

	copied := make([]GPUState, 0, len(p.gpus))
	for _, g := range p.gpus {
		copied = append(copied, GPUState{
			Index: g.Index, TaskID: g.TaskID, MemFree: g.MemFree, UtilPct: g.UtilPct,
		})
	}
	slices.SortFunc(copied, func(a, b GPUState) int { return a.Index - b.Index })
	return copied
}

// TotalCount returns the total number of GPUs in the pool.
func (p *GPUPool) TotalCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.gpus)
}

// FreeCount returns the number of unallocated GPUs.
func (p *GPUPool) FreeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for _, g := range p.gpus {
		if g.TaskID == "" {
			n++
		}
	}
	return n
}

func (p *GPUPool) Reserve(indices []int, taskID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, idx := range indices {
		g, ok := p.gpus[idx]
		if !ok {
			return fmt.Errorf("GPU %d not found in pool", idx)
		}
		if g.TaskID != "" {
			return fmt.Errorf("GPU %d already used by task %s", idx, g.TaskID)
		}
	}
	for _, idx := range indices {
		p.gpus[idx].TaskID = taskID
	}
	return nil
}

// externalTaskID is a sentinel value marking GPUs occupied by non-runq processes.
// Allocate skips GPUs with any non-empty TaskID, so this effectively blocks them.
const externalTaskID = "_external"

// RefreshExternalUsage calls nvidia-smi pmon to detect GPUs occupied by processes
// not managed by runq. Blocked GPUs are marked with "_external"; GPUs that were
// previously blocked but are now free get unmarked.
//
// allIndices: all GPU indices in the pool (for scanning).
// managedTaskIDs: set of taskIDs currently managed by runq (from the queue).
func (p *GPUPool) RefreshExternalUsage(procs []ResidualProcess, managedTaskIDs map[string]bool) (blocked, unblocked []int) {
	// Build set of GPU indices that have external processes.
	externalGPUs := make(map[int]bool)
	for _, proc := range procs {
		externalGPUs[proc.GPUIndex] = true
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for idx, g := range p.gpus {
		hasExternal := externalGPUs[idx]
		isManagedTask := g.TaskID != "" && g.TaskID != externalTaskID && managedTaskIDs[g.TaskID]

		if hasExternal && !isManagedTask && g.TaskID == "" {
			// Free GPU now has external process → block it.
			g.TaskID = externalTaskID
			blocked = append(blocked, idx)
		} else if !hasExternal && g.TaskID == externalTaskID {
			// Previously blocked GPU is now free → unblock it.
			g.TaskID = ""
			unblocked = append(unblocked, idx)
		}
		// If GPU has a managed task AND external process, leave it alone —
		// we don't block GPUs that runq is actively using.
	}
	return
}

func (p *GPUPool) sortedIndices() []int {
	indices := make([]int, 0, len(p.gpus))
	for idx := range p.gpus {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}
