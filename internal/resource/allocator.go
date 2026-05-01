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

	// Reserve marks specific GPU indices as allocated to taskID.
	// Used at daemon startup to restore running task assignments from DB.
	// Atomic: if any index is invalid or already occupied, no state is modified.
	Reserve(indices []int, taskID string) error
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

func (m *MockAllocator) Reserve(indices []int, taskID string) error {
	usedSet := make(map[int]bool)
	for _, idxs := range m.used {
		for _, idx := range idxs {
			usedSet[idx] = true
		}
	}
	for _, idx := range indices {
		if idx < 0 || idx >= m.Total {
			return fmt.Errorf("GPU index %d out of range [0, %d)", idx, m.Total)
		}
		if usedSet[idx] {
			return fmt.Errorf("GPU %d already allocated", idx)
		}
	}
	m.used[taskID] = append([]int{}, indices...)
	return nil
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
