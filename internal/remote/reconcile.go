// Reconcile algorithm: merges wrapper status.json + scheduler probe into a
// canonical task status. Pure logic, no I/O. Previously lived in hpccore.
package remote

import (
	"strings"

	"github.com/gliese129/runq-lab/internal/config"
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
func MapSignal(cfg *config.TargetConfig, token string) SchedulerSignal {
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

// evidenceClass is deliberately closed: every persisted/candidate provenance
// Reconcile understands belongs to exactly one class. The acceptance relation
// below compares these classes instead of relying on the order of observation
// branches, which would let a later weak probe regress an earlier terminal.
type evidenceClass uint8

const (
	evidenceNone evidenceClass = iota
	evidenceSubmit
	evidenceInferredTerminal
	evidenceSchedulerLive
	evidenceWrapperLive
	evidenceSchedulerTerminal
	evidenceWrapperTerminal
	evidenceRunqTerminal
)

func classifyEvidence(d Decision) evidenceClass {
	switch {
	case d.Source == SourceRunq && isTerminal(d.Status):
		return evidenceRunqTerminal
	case d.Source == SourceWrapper && isTerminal(d.Status):
		return evidenceWrapperTerminal
	case d.Source == SourceScheduler && isTerminal(d.Status):
		return evidenceSchedulerTerminal
	case d.Source == SourceInferred && isTerminal(d.Status):
		return evidenceInferredTerminal
	case d.Source == SourceWrapper && d.Status == "running":
		return evidenceWrapperLive
	case d.Source == SourceScheduler && (d.Status == "pending" || d.Status == "running"):
		return evidenceSchedulerLive
	case d.Source == SourceSubmit:
		return evidenceSubmit
	default:
		return evidenceNone
	}
}

// acceptCandidate is the complete evidence-acceptance relation.
//
//   - wrapper/runq terminals are hard and cannot be replaced;
//   - scheduler terminals are soft, but only hard terminal evidence can
//     correct them (another scheduler/live inference cannot regress them);
//   - inferred and submit terminals are provisional and remain correctable;
//   - wrapper-live outranks scheduler-live, while the combined wrapper-live +
//     scheduler-gone inference may still detect a vanished process;
//   - scheduler-live may advance pending -> running, never running -> pending.
func acceptCandidate(current, candidate Decision) bool {
	currentEvidence := classifyEvidence(current)
	candidateEvidence := classifyEvidence(candidate)

	switch currentEvidence {
	case evidenceWrapperTerminal, evidenceRunqTerminal:
		return false
	case evidenceSchedulerTerminal:
		switch candidateEvidence {
		case evidenceWrapperTerminal, evidenceRunqTerminal:
			return true
		default:
			return false
		}
	case evidenceWrapperLive:
		switch candidateEvidence {
		case evidenceInferredTerminal, evidenceWrapperLive, evidenceSchedulerTerminal,
			evidenceWrapperTerminal, evidenceRunqTerminal:
			return true
		default:
			return false
		}
	case evidenceSchedulerLive:
		switch candidateEvidence {
		case evidenceInferredTerminal, evidenceWrapperLive, evidenceSchedulerTerminal,
			evidenceWrapperTerminal, evidenceRunqTerminal:
			return true
		case evidenceSchedulerLive:
			return current.Status != "running" || candidate.Status != "pending"
		default:
			return false
		}
	case evidenceNone, evidenceSubmit, evidenceInferredTerminal:
		return candidateEvidence != evidenceNone && candidateEvidence != evidenceSubmit
	default:
		return false
	}
}

func observedCandidate(o Observed) (Decision, bool) {
	if o.KillRequested {
		return Decision{"killed", SourceRunq}, true
	}
	if o.WrapperStatus == "success" || o.WrapperStatus == "failed" || o.WrapperStatus == "killed" {
		return Decision{o.WrapperStatus, SourceWrapper}, true
	}
	switch o.Scheduler {
	case SchedSuccess:
		return Decision{"success", SourceScheduler}, true
	case SchedFailed:
		return Decision{"failed", SourceScheduler}, true
	case SchedKilled:
		return Decision{"killed", SourceScheduler}, true
	}
	if o.WrapperStatus == "started" || o.WrapperStatus == "running" {
		if o.Scheduler == SchedGone {
			return Decision{"failed", SourceInferred}, true
		}
		return Decision{"running", SourceWrapper}, true
	}
	switch o.Scheduler {
	case SchedRunning:
		return Decision{"running", SourceScheduler}, true
	case SchedActive, SchedPending:
		return Decision{"pending", SourceScheduler}, true
	default:
		return Decision{}, false
	}
}

// Reconcile merges wrapper (process-inside) and scheduler (process-outside)
// facts into the canonical status to persist, plus provenance.
//
// Precedence:
//
//	KillRequested                              → killed   / runq
//	wrapper success|failed|killed              → that     / wrapper
//	scheduler Success|Failed|Killed            → that     / scheduler
//	wrapper started|running + scheduler Gone   → failed   / inferred
//	wrapper started|running                    → running  / wrapper
//	wrapper "" + scheduler Running             → running  / scheduler
//	wrapper "" + scheduler Active|Pending      → pending  / scheduler
//	otherwise                                  → keep current
func Reconcile(currentStatus, currentSource string, o Observed) Decision {
	current := Decision{Status: currentStatus, Source: currentSource}
	candidate, ok := observedCandidate(o)
	if !ok || !acceptCandidate(current, candidate) {
		return current
	}
	return candidate
}
