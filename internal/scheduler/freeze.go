package scheduler

import (
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bytedance/gopkg/util/logger"
)

// FreezeEvent records one entry into the frozen state. Multiple events can
// accumulate during a single freeze cycle if more tasks trigger disk_low (or
// any other reason) before the user thaws — each call to Freeze() appends a
// new FreezeEvent so the audit trail is preserved even when Reason strings
// match.
//
// Webhook (L2-D) and admin tooling read Events() to render the full timeline.
type FreezeEvent struct {
	Reason        string    `json:"reason"`                    // "disk_low" / "manual" / "passive_check" / ...
	TriggerTaskID string    `json:"trigger_task_id,omitempty"` // who told us; informational only
	EnteredAt     time.Time `json:"entered_at"`                // FreezeState auto-stamps if caller leaves zero

	// Disk-specific context, populated when Reason == "disk_low".
	DiskMount string `json:"disk_mount,omitempty"` // mountpoint of the affected disk
	FreeBytes int64  `json:"free_bytes,omitempty"`
	NeededEst int64  `json:"needed_est,omitempty"`
}

// FrozenTask is the per-task record kept inside FreezeState while a task is
// SIGSTOPped. All four fields are populated at Freeze() time and never
// mutated; partial thaw / force thaw / manual remove all just delete entries.
//
// Fields:
//   - PID: pgroup leader pid (executor sets Setpgid:true, so pgid == leader pid)
//   - Mount: mountpoint of the task's checkpoint dir; used to group tasks by
//     disk for per-mount partial thaw on multi-disk machines
//   - NeededBytes: free bytes required on Mount before this task can safely
//     resume. SDK reports this at freeze time as upcoming-ckpt-size × safety
//     factor; ThawTasks uses the same value as the per-task threshold (so
//     freeze and thaw decisions can never drift)
//   - JobID: parent job; lets the scheduler skip the rest of this job's
//     pending tasks (they share the same ckpt pattern, would likely also bomb)
type FrozenTask struct {
	PID         int    `json:"pid"`
	Mount       string `json:"mount"`
	NeededBytes int64  `json:"needed_bytes"`
	JobID       string `json:"job_id"`
}

// BlockReason explains why a particular task stayed frozen during a checked
// thaw. FreeBytes == -1 means the disk usage call itself failed.
type BlockReason struct {
	Mount     string `json:"mount"`
	FreeBytes int64  `json:"free_bytes"`
	Threshold int64  `json:"threshold"`
}

// ThawResult is what ThawTasks returns: which tasks made it out, which are
// still frozen and why.
type ThawResult struct {
	Thawed  []string               `json:"thawed"`
	Blocked map[string]BlockReason `json:"blocked,omitempty"`
}

// FreezeState is the disk-freeze state machine.
//
// L2-C stage 1 design (SDK-driven):
//   - SDK self-detects low disk before each save and POSTs
//     /api/internal/freeze-self to daemon with task_id + free_bytes +
//     needed_est + mount. The SDK is the ONLY freeze trigger in stage 1.
//   - Daemon constructs a FrozenTask and calls Freeze() with a single-entry
//     map. FreezeState SIGSTOPs that task's pgroup and records it.
//   - Other running tasks are NOT touched by daemon. They self-detect at
//     their own save time. Cross-task daemon-side filtering is intentionally
//     absent — non-SDK users get zero freeze magic, which is fine: they'll
//     crash on ENOSPC same as without runq, and that's their contract.
//   - Manual `runq freeze` and daemon passive_check are future modes;
//     signatures keep `tasks` a map rather than a single entry to allow them.
//
// State is derived: IsFrozen() iff len(frozenTasks) > 0. No separate flag.
// Auto-thaw-on-last-removed is trivial: when RemoveTask clears the last
// entry, IsFrozen() naturally returns false and the scheduler resumes.
//
// Release paths:
//   - autoThawLoop: scheduler polls disk usage per mount, calls ThawTasks
//     for all frozen tasks; per-task NeededBytes decides who gets through.
//   - manual `runq thaw`: API handler filters to caller-owned tasks, calls
//     ThawTasks (checked) or ThawForce (--force flag).
//   - task exit / kill: scheduler calls RemoveTask in runTask's defer.
//
// Kill-during-freeze: SIGTERM/SIGKILL penetrate SIGSTOP semantics, so
// `runq kill` works on a frozen task. The exit path triggers RemoveTask.
//
// IMPORTANT:
//   - SIGSTOP/SIGCONT MUST target the process group (-pgid), not the bare
//     pid. The executor guarantees Setpgid:true so pgid == leader pid; we
//     rely on that invariant here.
//   - `host` is informational only (webhook payload / log context).
//     Multi-host coordination is explicitly out of scope — labs with
//     >1 box use the HPC backend (L2-E) instead.
type FreezeState struct {
	mu          sync.RWMutex
	frozenTasks map[string]FrozenTask // taskID → record; empty ⇔ unfrozen
	events      []FreezeEvent         // append-only during this freeze cycle; cleared when frozenTasks drains
	host        string                // os.Hostname() at construction; informational only
}

// NewFreezeState returns an unfrozen FreezeState scoped to the current host.
func NewFreezeState() *FreezeState {
	host, _ := os.Hostname()
	return &FreezeState{
		frozenTasks: make(map[string]FrozenTask),
		host:        host,
	}
}

// IsFrozen reports whether scheduling is currently paused due to a freeze.
// Cheap; safe to call from the scheduler tick.
func (f *FreezeState) IsFrozen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.frozenTasks) > 0
}

// Host returns the hostname of the daemon. Informational only — useful for
// webhook payloads ("freeze on box-a") and log context. No multi-host logic
// is implemented; labs with multiple boxes use the HPC backend instead.
func (f *FreezeState) Host() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.host
}

// Events returns a snapshot of all events recorded during the current freeze
// cycle (cleared when frozenTasks drains). Most recent last.
func (f *FreezeState) Events() []FreezeEvent {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]FreezeEvent, len(f.events))
	copy(out, f.events)
	return out
}

// LatestEvent returns the most recently appended FreezeEvent, or nil if not
// frozen.
func (f *FreezeState) LatestEvent() *FreezeEvent {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.events) == 0 {
		return nil
	}
	ev := f.events[len(f.events)-1]
	return &ev
}

// FirstFrozenAt returns the EnteredAt of the first event in the current
// cycle. Zero when not frozen.
func (f *FreezeState) FirstFrozenAt() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.events) == 0 {
		return time.Time{}
	}
	return f.events[0].EnteredAt
}

// FrozenTaskIDs returns a snapshot of currently frozen task IDs.
func (f *FreezeState) FrozenTaskIDs() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, 0, len(f.frozenTasks))
	for id := range f.frozenTasks {
		out = append(out, id)
	}
	return out
}

// TriggerTaskIDs returns the deduplicated set of TriggerTaskID values across
// all events in the current cycle. Useful for webhook summaries.
func (f *FreezeState) TriggerTaskIDs() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	seen := make(map[string]struct{}, len(f.events))
	out := make([]string, 0, len(f.events))
	for _, ev := range f.events {
		if ev.TriggerTaskID == "" {
			continue
		}
		if _, ok := seen[ev.TriggerTaskID]; ok {
			continue
		}
		seen[ev.TriggerTaskID] = struct{}{}
		out = append(out, ev.TriggerTaskID)
	}
	return out
}

// FrozenJobs returns the set of job IDs that have at least one frozen task.
// scheduler.tick uses this to skip pending tasks belonging to a frozen job —
// sibling tasks share the same ckpt pattern and would likely also bomb if
// dispatched.
func (f *FreezeState) FrozenJobs() map[string]struct{} {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]struct{})
	for _, info := range f.frozenTasks {
		if info.JobID != "" {
			out[info.JobID] = struct{}{}
		}
	}
	return out
}

// FrozenMounts returns the set of mountpoints with at least one frozen task.
// scheduler.tick uses this to refuse dispatching new tasks that would write
// to an already-stressed disk. Multi-disk machines benefit: dispatching to
// healthy mounts continues.
func (f *FreezeState) FrozenMounts() map[string]struct{} {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]struct{})
	for _, info := range f.frozenTasks {
		if info.Mount != "" {
			out[info.Mount] = struct{}{}
		}
	}
	return out
}

// SnapshotFrozenTasks returns a copy of the frozen-task table. autoThawLoop
// uses this to compute per-mount and per-task thresholds without holding the
// lock.
func (f *FreezeState) SnapshotFrozenTasks() map[string]FrozenTask {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]FrozenTask, len(f.frozenTasks))
	for k, v := range f.frozenTasks {
		out[k] = v
	}
	return out
}

// Freeze SIGSTOPs the listed tasks' process groups and records them.
//
// Each call appends `ev` to the events list — repeated triggers with the
// same Reason are preserved as distinct events even if Reason strings
// collide. Best-effort: per-task syscall errors are logged and skipped,
// never abort the operation. The event is appended unconditionally so the
// audit trail records that we tried, even if every SIGSTOP failed.
//
// `tasks` is taskID → FrozenTask. Caller (api handler) is responsible for
// populating PID/Mount/NeededBytes/JobID before calling. FreezeState does
// NOT look up tasks or decide which to freeze — it only executes.
//
// No special-case for "trigger task already SIGSTOPped" — in stage 1 the
// SDK does NOT self-SIGSTOP; daemon is the only SIGSTOP source.
func (f *FreezeState) Freeze(ev FreezeEvent, tasks map[string]FrozenTask) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if ev.EnteredAt.IsZero() {
		ev.EnteredAt = time.Now()
	}
	f.events = append(f.events, ev)

	for tid, info := range tasks {
		if info.PID <= 0 {
			logger.Warnf("freeze: skipping task %s with invalid pid %d", tid, info.PID)
			continue
		}
		pgid, err := syscall.Getpgid(info.PID)
		if err != nil {
			logger.Warnf("freeze: Getpgid(%d) for task %s failed: %v", info.PID, tid, err)
			continue
		}
		if err := syscall.Kill(-pgid, syscall.SIGSTOP); err != nil {
			logger.Warnf("freeze: SIGSTOP pgid=%d task=%s failed: %v", pgid, tid, err)
			continue
		}
		f.frozenTasks[tid] = info
	}
}

// DiskFreeMargin is the buffer added to NeededBytes when deciding whether
// a mount has recovered enough for a task to resume. Reason: disk.Usage()
// and the kernel's statfs() can disagree by a few filesystem pages due to
// rounding, cached writes, etc. Checkpoints are GB-scale so a 1 MB margin
// is plenty without delaying recovery noticeably.
const DiskFreeMargin = 1 * 1024 * 1024

// ThawTasks attempts to resume the listed tasks with per-task disk-safety
// checks. Each mount is inspected once via diskUsage; a task passes only
// when free >= NeededBytes + DiskFreeMargin. Blocked tasks appear in
// result.Blocked with a per-task reason; thawed (or already-departed)
// tasks appear in result.Thawed.
//
// Caller is responsible for owner-scoping `ids`. FreezeState does not
// enforce ownership.
//
// Symmetry: NeededBytes was set at freeze time from the SDK-reported
// upcoming-ckpt-size × safety factor. ThawTasks reuses the same value as
// the per-task threshold, so freeze and thaw decisions cannot drift.
//
// "Already-departed" tasks (PID gone between freeze and thaw) are treated
// as thawed from the state-machine view — they leave frozenTasks and land
// in result.Thawed. Caller can distinguish via BlockReason.FreeBytes == -1
// when needed (that's the "couldn't stat" signal, not "departed").
func (f *FreezeState) ThawTasks(
	ids []string,
	diskUsage func(mount string) (uint64, error),
) ThawResult {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Group by mount so diskUsage runs once per mount (NFS / disk.Usage can
	// be slow on some filesystems). Unknown IDs are silently dropped — the
	// caller's snapshot may simply be outdated.
	byMount := make(map[string][]string)
	for _, id := range ids {
		t, ok := f.frozenTasks[id]
		if !ok {
			continue
		}
		byMount[t.Mount] = append(byMount[t.Mount], id)
	}

	result := ThawResult{Blocked: map[string]BlockReason{}}

	for mount, tids := range byMount {
		freeU64, err := diskUsage(mount)
		var free int64 = -1 // sentinel: -1 means "stat failed"
		if err == nil {
			free = int64(freeU64)
		} else {
			logger.Warnf("thaw: diskUsage(%q) failed: %v", mount, err)
		}

		for _, tid := range tids {
			info := f.frozenTasks[tid]

			// Block when disk lookup failed OR free is below the per-task
			// threshold. The +Margin guards against statfs/usage rounding.
			if free < info.NeededBytes+DiskFreeMargin {
				result.Blocked[tid] = BlockReason{
					Mount:     mount,
					FreeBytes: free,
					Threshold: info.NeededBytes,
				}
				continue
			}

			// Free is sufficient — send SIGCONT (best-effort) and remove
			// the entry from frozenTasks regardless of syscall outcome.
			// State-machine view: "thawed" means "not in frozenTasks". A
			// dead pgroup or failed SIGCONT doesn't change that.
			if pgid, err := syscall.Getpgid(info.PID); err != nil {
				logger.Warnf("thaw: Getpgid(%d) task=%s failed: %v (treating as departed)", info.PID, tid, err)
			} else if err := syscall.Kill(-pgid, syscall.SIGCONT); err != nil {
				logger.Warnf("thaw: SIGCONT pgid=%d task=%s failed: %v", pgid, tid, err)
			}
			delete(f.frozenTasks, tid)
			result.Thawed = append(result.Thawed, tid)
		}
	}

	if len(f.frozenTasks) == 0 {
		f.events = nil
	}
	return result
}

// ThawForce SIGCONTs the listed tasks unconditionally — no disk check.
// Used by `runq thaw --force` when the user accepts they may immediately
// re-trigger a freeze. Owner-scope still applies (caller pre-filters);
// force only bypasses the per-task disk safety check.
//
// Returned slice is the IDs that came out. May be smaller than `ids` if
// some entries were stale (already removed by RemoveTask between the
// caller's lookup and this call). Stale IDs are silently dropped — the
// caller asked for those to be unfrozen, and they already are.
//
// Best-effort: per-task syscall errors are logged and the entry still
// gets deleted + reported as thawed. Rationale: caller wanted out and
// the state-machine view of "thawed" is "no longer in frozenTasks"; a
// dead pgroup or failed SIGCONT doesn't change that.
func (f *FreezeState) ThawForce(ids []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	resumed := make([]string, 0, len(ids))
	for _, id := range ids {
		info, ok := f.frozenTasks[id]
		if !ok {
			// Stale ID — caller's snapshot was outdated; nothing to do.
			continue
		}
		// SIGCONT is best-effort: if pgroup is already gone (task died
		// while frozen) or SIGCONT fails for any reason, the entry still
		// leaves frozenTasks. Caller asked for --force, they get force.
		if info.PID > 0 {
			pgid, err := syscall.Getpgid(info.PID)
			if err != nil {
				logger.Warnf("thaw-force: Getpgid(%d) for task %s failed: %v (treating as already gone)", info.PID, id, err)
			} else if err := syscall.Kill(-pgid, syscall.SIGCONT); err != nil {
				logger.Warnf("thaw-force: SIGCONT pgid=%d task=%s failed: %v", pgid, id, err)
			}
		}
		delete(f.frozenTasks, id)
		resumed = append(resumed, id)
	}
	if len(f.frozenTasks) == 0 {
		f.events = nil
	}
	return resumed
}

// RemoveTask is called by the scheduler when a frozen task exits (killed by
// the user via SIGTERM, OOM, or anything else). Pure state cleanup; no
// signals are sent. When the last frozen task is removed, IsFrozen() flips
// to false and events drain — scheduler resumes naturally on its next tick.
func (f *FreezeState) RemoveTask(taskID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.frozenTasks, taskID)
	if len(f.frozenTasks) == 0 {
		f.events = nil
	}
}
