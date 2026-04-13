package scheduler

import (
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/runq/runq/internal/gpu"
)

// GPUState tracks the status of a single GPU.
type GPUState struct {
	Index   int
	TaskID  string // empty if free
	MemFree int    // MB, from nvidia-smi
	UtilPct int    // %, from nvidia-smi
}

// GPUPool manages GPU allocation on the local machine.
// Thread-safe: all public methods acquire mu.
type GPUPool struct {
	mu   sync.Mutex
	gpus map[int]*GPUState
}

// NewGPUPool initializes a GPUPool from detected GPU info.
func NewGPUPool(infos []gpu.Info) *GPUPool {
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

// Allocate tries to assign n free GPUs to the given taskID.
// Returns the allocated GPU indices (sorted), or error if not enough are free.
// On error, no state is modified (atomic: all-or-nothing).
func (p *GPUPool) Allocate(n int, taskID string) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Collect free GPUs in deterministic order.
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

// Release frees all GPUs assigned to the given taskID.
// Idempotent: no error if taskID is not found.
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
// Returns copies — callers cannot mutate pool state.
func (p *GPUPool) Status() []GPUState {
	p.mu.Lock()
	defer p.mu.Unlock()

	copied := make([]GPUState, 0, len(p.gpus))
	for _, g := range p.gpus {
		copied = append(copied, GPUState{
			Index:   g.Index,
			TaskID:  g.TaskID,
			MemFree: g.MemFree,
			UtilPct: g.UtilPct,
		})
	}
	slices.SortFunc(copied, func(a, b GPUState) int {
		return a.Index - b.Index
	})
	return copied
}

// FreeCount returns the number of currently unallocated GPUs.
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

// sortedIndices returns all GPU indices in sorted order.
// Caller must hold mu.
func (p *GPUPool) sortedIndices() []int {
	indices := make([]int, 0, len(p.gpus))
	for idx := range p.gpus {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}
