package resource

import "fmt"

// Allocator abstracts GPU allocation for the scheduler.
// Production uses *GPUPool; tests use MockAllocator.
type Allocator interface {
	Allocate(n int, taskID string) ([]int, error)
	Release(taskID string)
	FreeCount() int
	TotalCount() int
	Status() []GPUState
}

// MockAllocator is a test double that simulates GPU allocation without real hardware.
type MockAllocator struct {
	Total int
	used  map[string][]int // taskID → GPU indices
}

func NewMockAllocator(n int) *MockAllocator {
	return &MockAllocator{Total: n, used: make(map[string][]int)}
}

func (m *MockAllocator) Allocate(n int, taskID string) ([]int, error) {
	free := m.FreeCount()
	if free < n {
		return nil, fmt.Errorf("not enough free GPUs: requested %d, available %d", n, free)
	}
	// Assign sequential indices starting from first free.
	usedSet := make(map[int]bool)
	for _, indices := range m.used {
		for _, idx := range indices {
			usedSet[idx] = true
		}
	}
	var allocated []int
	for i := 0; i < m.Total && len(allocated) < n; i++ {
		if !usedSet[i] {
			allocated = append(allocated, i)
		}
	}
	m.used[taskID] = allocated
	return allocated, nil
}

func (m *MockAllocator) Release(taskID string) {
	delete(m.used, taskID)
}

func (m *MockAllocator) FreeCount() int {
	usedCount := 0
	for _, indices := range m.used {
		usedCount += len(indices)
	}
	return m.Total - usedCount
}

func (m *MockAllocator) TotalCount() int {
	return m.Total
}

func (m *MockAllocator) Status() []GPUState {
	// Build a task map for quick lookup.
	taskMap := make(map[int]string)
	for taskID, indices := range m.used {
		for _, idx := range indices {
			taskMap[idx] = taskID
		}
	}
	states := make([]GPUState, m.Total)
	for i := 0; i < m.Total; i++ {
		states[i] = GPUState{Index: i, TaskID: taskMap[i], MemFree: 80000}
	}
	return states
}
