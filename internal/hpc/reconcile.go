// Reconcile algorithm: merges wrapper status.json + scheduler probe into a
// canonical task status. Pure logic, no I/O. Previously lived in hpccore.
package hpc

import (
	"strings"

	"github.com/gliese129/runq/internal/hpcconfig"
)

// SchedulerSignal is the semantic verdict about a task derived from the
// scheduler, NOT a raw scheduler string.
type SchedulerSignal string

const (
	SchedUnknown SchedulerSignal = "unknown" // no info: not configured, no ext id, probe/parse error
	SchedActive  SchedulerSignal = "active"  // present in the scheduler, pending-vs-running unspecified
	SchedPending SchedulerSignal = "pending" // confirmed queued
	SchedRunning SchedulerSignal = "running" // confirmed running
	SchedGone    SchedulerSignal = "gone"    // probe succeeded but task absent from the active query
	SchedSuccess SchedulerSignal = "success" // accounting: completed OK
	SchedFailed  SchedulerSignal = "failed"  // accounting: failed/timeout/oom/node_fail
	SchedKilled  SchedulerSignal = "killed"  // accounting: cancelled
)

// ProbeResult holds the full output of a scheduler probe: the semantic signal
// for Reconcile, plus raw strings for display in CLI/dashboard.
type ProbeResult struct {
	Signal      SchedulerSignal
	NativeState string // raw state token before signal mapping (e.g. "CONFIGURING")
	Queue       string // scheduler queue/partition, if available
}

// MapSignal maps a raw state token to a SchedulerSignal using the config's
// SignalMap first (user-configurable, case-insensitive), then falls back to
// the hardcoded ParseSignal. This keeps scheduler-specific knowledge in
// config presets rather than Go code.
func MapSignal(cfg *hpcconfig.Config, token string) SchedulerSignal {
	if cfg != nil && len(cfg.SignalMap) > 0 {
		upper := strings.ToUpper(strings.TrimSpace(token))
		for k, v := range cfg.SignalMap {
			if strings.ToUpper(k) == upper {
				return ParseSignal(v)
			}
		}
	}
	return ParseSignal(token)
}

// ParseSignal maps a normalized token to a SchedulerSignal. Recognizes
// canonical tokens plus Slurm sacct vocabulary as a convenience.
func ParseSignal(token string) SchedulerSignal {
	switch strings.ToUpper(strings.TrimSpace(token)) {
	case "PENDING":
		return SchedPending
	case "RUNNING":
		return SchedRunning
	case "SUCCESS", "COMPLETED":
		return SchedSuccess
	case "FAILED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "BOOT_FAIL", "DEADLINE":
		return SchedFailed
	case "KILLED", "CANCELLED":
		return SchedKilled
	case "GONE":
		return SchedGone
	default:
		return SchedUnknown
	}
}

// Status-source values: provenance of a canonical status, persisted in
// tasks.status_source. "" = unknown.
const (
	SourceWrapper   = "wrapper"   // from the wrapper's status.json
	SourceScheduler = "scheduler" // from the scheduler probe
	SourceInferred  = "inferred"  // a guess: wrapper said running but scheduler says gone
	SourceRunq      = "runq"      // a runq-initiated kill that succeeded
	SourceSubmit    = "submit"    // the submit command itself failed
)

// Decision is Reconcile's output: canonical status plus where it came from.
type Decision struct {
	Status string
	Source string
}

// Observed is the multi-source view of a single task that Reconcile collapses
// into one canonical status.
type Observed struct {
	WrapperStatus string
	ExitCode      *int
	Scheduler     SchedulerSignal
	KillRequested bool
}

// Reconcile merges wrapper (process-inside) and scheduler (process-outside)
// facts into the canonical status to persist, plus provenance.
//
// Precedence:
//
//	KillRequested                              → killed   / runq
//	wrapper success|failed                     → that     / wrapper
//	scheduler Success|Failed|Killed            → that     / scheduler
//	wrapper started|running + scheduler Gone   → failed   / inferred
//	wrapper started|running                    → running  / wrapper
//	wrapper "" + scheduler Running             → running  / scheduler
//	wrapper "" + scheduler Active|Pending      → pending  / scheduler
//	otherwise                                  → keep current
func Reconcile(currentStatus, currentSource string, o Observed) Decision {
	keep := Decision{Status: currentStatus, Source: currentSource}

	if o.KillRequested {
		return Decision{"killed", SourceRunq}
	}
	if o.WrapperStatus == "success" || o.WrapperStatus == "failed" {
		return Decision{o.WrapperStatus, SourceWrapper}
	}
	switch o.Scheduler {
	case SchedSuccess:
		return Decision{"success", SourceScheduler}
	case SchedFailed:
		return Decision{"failed", SourceScheduler}
	case SchedKilled:
		return Decision{"killed", SourceScheduler}
	}
	if o.WrapperStatus == "started" || o.WrapperStatus == "running" {
		if o.Scheduler == SchedGone {
			return Decision{"failed", SourceInferred}
		}
		return Decision{"running", SourceWrapper}
	}
	switch o.Scheduler {
	case SchedRunning:
		return Decision{"running", SourceScheduler}
	case SchedActive, SchedPending:
		return Decision{"pending", SourceScheduler}
	}
	return keep
}
