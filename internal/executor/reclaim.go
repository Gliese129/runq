package executor

// Reclaim handles daemon-restart recovery.
//
// On daemon start, it reads all tasks with status=running from the store,
// then attempts to reclaim each one by:
//  1. Checking if the PID is still alive (os.FindProcess + signal 0)
//  2. Validating /proc/<pid>/stat starttime matches stored value
//  3. If valid: re-attach monitoring. If stale: mark as crashed for retry.
//
// TODO: implement
type Reclaimer struct {
	// TODO: add dependencies
}
