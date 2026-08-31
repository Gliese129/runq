package store

// attemptLockShards coordinate non-transactional attempt effects across lane
// generations that share this Store. Durable status writes still use SQL CAS;
// these process-local locks cover the filesystem-read/ingest boundary that a
// SQL predicate cannot make atomic with a wrapper reset.
const attemptLockShards = 64

// WithTaskAttemptLock serializes one task's attempt-file effects. Sharding
// keeps the lock set bounded while allowing unrelated tasks to ingest/reset
// concurrently.
func (s *Store) WithTaskAttemptLock(taskID string, fn func() error) error {
	var hash uint32 = 2166136261
	for i := 0; i < len(taskID); i++ {
		hash ^= uint32(taskID[i])
		hash *= 16777619
	}
	mu := &s.attemptLocks[hash%attemptLockShards]
	mu.Lock()
	defer mu.Unlock()
	return fn()
}
