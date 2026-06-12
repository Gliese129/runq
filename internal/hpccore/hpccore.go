// Package hpccore holds the three pure algorithms at the heart of the HPC
// backend. It has no I/O and depends on nothing else in the codebase (only the
// standard library), so it can be unit-tested in isolation with table-driven
// tests.
//
// OWNERSHIP: the function BODIES in this file are stubs. They are meant to be
// implemented by the algorithm owner — the glue in internal/hpc only calls
// these signatures. Each stub panics / returns a sentinel so a forgotten
// implementation fails loudly rather than silently shipping a weak version
// (ShellQuote in particular is security-sensitive).
//
// See L2E-layer2-hpc-spec.md §2 for the full contracts.
package hpccore

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrNotImplemented is returned by stubbed functions until they are filled in.
var ErrNotImplemented = errors.New("hpccore: not implemented")

// ShellQuote turns an arbitrary string into a single POSIX-shell-safe token.
//
// THREAT MODEL: values passed here are untrusted — sweep parameter values,
// user environment values, and text parsed out of external scheduler output.
// A value like `0.1; rm -rf ~` must NOT be able to break out of its token and
// inject commands. The classic safe form wraps the value in single quotes and
// escapes any embedded single quote as '\” (close-quote, escaped-quote,
// reopen-quote). Implement that here; this is the security primitive every
// other interpolation builds on.
//
// Returns a string (no error) so it composes cleanly inside Render.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	s = strings.ReplaceAll(s, "'", `'\''`)
	return "'" + s + "'"
}

// ParamRegex matches {{name}} placeholders. Dots are allowed so namespaced
// vars like {{param.h_rt}} (task params exposed to submit_template) resolve;
// templates whose var set lacks the key still fail closed as before.
const ParamRegex = `\{\{\s*([\w.\-]+)\s*}}`

// Render fills {{name}} placeholders in tmpl with the matching value from vars.
// Every substituted value MUST pass through ShellQuote so the rendered string
// is safe to hand to a shell.
//
// Contract:
//   - Placeholder syntax is {{name}} (define exact whitespace handling).
//   - An unknown placeholder (no matching key in vars) is a hard error
//     (fail closed) — never leave a literal {{...}} or silently blank it.
//   - A key in vars with no placeholder is ignored (not an error).
//
// Used by the glue for two things: the cluster command templates
// (submit/status/kill) and the `export KEY=<value>` lines of run.sh.
func Render(tmpl string, vars map[string]string) (string, error) {
	reg := regexp.MustCompile(ParamRegex)
	var err error
	result := reg.ReplaceAllStringFunc(tmpl, func(match string) string {
		submatches := reg.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		param := submatches[1]
		val, ok := vars[param]
		if !ok {
			err = fmt.Errorf("param %s not found", param)
			return match
		}
		return ShellQuote(val)
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// ExtractSubmitID pulls the external scheduler job id out of a submit command's
// stdout/stderr using the user-configured regex.
//
// Contract:
//   - regex is expected to contain exactly one capture group = the id.
//   - On a match, return the first capture group.
//   - No match → error, so a failed/garbled submission surfaces instead of
//     silently recording an empty id.
func ExtractSubmitID(output, regex string) (string, error) {
	reg, err := regexp.Compile(regex)
	if err != nil {
		return "", fmt.Errorf("invalid submit_id_regex %q: %w", regex, err)
	}
	result := reg.FindAllStringSubmatch(output, 1)
	if len(result) == 0 {
		return "", fmt.Errorf("no match found")
	}
	if len(result[0]) <= 1 {
		return "", fmt.Errorf("no sub group found")
	}
	return result[0][1], nil
}

// SchedulerSignal is the semantic verdict about a task derived from the
// scheduler, NOT a raw scheduler string. Modeling it as an enum is what kills
// the old `SchedulerState == ""` ambiguity (which conflated "not probed" with
// "job gone"). The glue produces it from the status_template probe and the
// optional status_parser hook (see ParseSignal); Reconcile merges it with the
// wrapper's own status.
//
// Liveness values (Active/Pending/Running/Gone) come from an active query like
// squeue; terminal values (Success/Failed/Killed) only appear when a user
// configures a parser hook that reads accounting (e.g. sacct). runq ships no
// built-in dialect parser — the hook is the extension point.
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

// ParseSignal maps a normalized token (the output of status_template, possibly
// transformed by a status_parser hook) to a SchedulerSignal. It recognizes the
// canonical tokens plus Slurm's sacct vocabulary as a convenience, so a Slurm
// user can point status_template straight at sacct without a hook. Anything
// unrecognized → SchedUnknown; the caller decides what an unrecognized-but-
// present value means (see the glue: present → SchedActive). PBS/SGE/etc. that
// don't emit these tokens use a status_parser hook to translate.
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

// Status-source values: the provenance of a canonical status, persisted in
// tasks.status_source. They let the HPC backend treat an "inferred" terminal as
// correctable while wrapper/scheduler/runq terminals are final. "" = unknown.
const (
	SourceWrapper   = "wrapper"   // from the wrapper's status.json
	SourceScheduler = "scheduler" // from the scheduler probe (active state or accounting terminal)
	SourceInferred  = "inferred"  // a guess: wrapper said running but the scheduler says it's gone
	SourceRunq      = "runq"      // a runq-initiated kill that succeeded
	SourceSubmit    = "submit"    // the submit command itself failed
)

// Decision is Reconcile's output: the canonical status plus where it came from.
type Decision struct {
	Status string
	Source string
}

// Observed is the multi-source view of a single task that Reconcile collapses
// into one canonical status. The glue fills this from status.json plus the
// optional scheduler probe.
type Observed struct {
	// WrapperStatus is the `status` field from <task_dir>/status.json, written
	// by run.sh. One of: "" (no file yet) | "started" | "running" | "success" |
	// "failed".
	WrapperStatus string
	// ExitCode is set by the wrapper on terminal states (success/failed).
	ExitCode *int
	// Scheduler is the semantic verdict from the scheduler probe. SchedUnknown
	// when not configured / no ext id / probe failed — meaning "no new fact".
	Scheduler SchedulerSignal
	// KillRequested is set by the glue when the user has run `hpc kill` for this
	// task (DB stays the source of truth).
	KillRequested bool
}

// Reconcile MERGES two independent fact sources into the canonical status to
// persist — the wrapper's own exit code (process-inside truth) and the
// scheduler's verdict (process-outside lifecycle) — and reports the provenance
// (Source) so the backend knows how much to trust it. The DB is the source of
// truth; Reconcile takes the current status+source and returns what to write,
// keeping current unchanged when there is no new fact, so a transient missing
// observation never downgrades a task (e.g. running → pending).
//
// Precedence (status / source):
//
//	KillRequested                                  -> killed   / runq
//	wrapper success|failed                         -> that     / wrapper   (own exit code, most trustworthy)
//	scheduler Success|Failed|Killed                -> that     / scheduler (accounting; deaths the wrapper couldn't record)
//	wrapper started|running + scheduler Gone       -> failed   / inferred  (zombie GUESS — correctable later)
//	wrapper started|running                        -> running  / wrapper
//	wrapper "" + scheduler Running                 -> running  / scheduler
//	wrapper "" + scheduler Active|Pending          -> pending  / scheduler
//	otherwise (incl. wrapper "" + Gone/Unknown)    -> keep current status+source
//
// Note: "wrapper "" + Gone" deliberately KEEPS current (does not infer failure).
// We only infer death when we have positive evidence the task actually ran
// (wrapper started/running); a task that never reported running must not be
// killed off just because an active query doesn't list it (it may be in the
// submit→start race window, or simply not written status.json yet).
func Reconcile(currentStatus, currentSource string, o Observed) Decision {
	keep := Decision{Status: currentStatus, Source: currentSource}

	if o.KillRequested {
		return Decision{"killed", SourceRunq}
	}
	// Wrapper's own terminal exit code wins — it has the real result.
	if o.WrapperStatus == "success" || o.WrapperStatus == "failed" {
		return Decision{o.WrapperStatus, SourceWrapper}
	}
	// Scheduler-reported terminal states (only when a parser hook emits them,
	// e.g. sacct). Covers SIGKILL/OOM/timeout/node-fail the wrapper couldn't record.
	switch o.Scheduler {
	case SchedSuccess:
		return Decision{"success", SourceScheduler}
	case SchedFailed:
		return Decision{"failed", SourceScheduler}
	case SchedKilled:
		return Decision{"killed", SourceScheduler}
	}
	// Wrapper reports it is executing.
	if o.WrapperStatus == "started" || o.WrapperStatus == "running" {
		if o.Scheduler == SchedGone {
			// Ran, but the scheduler confirms it's gone with no terminal record:
			// a zombie GUESS. Mark failed/inferred so a later wrapper terminal can
			// still correct it.
			return Decision{"failed", SourceInferred}
		}
		return Decision{"running", SourceWrapper}
	}
	// No wrapper signal yet — lean on the scheduler's liveness only.
	switch o.Scheduler {
	case SchedRunning:
		return Decision{"running", SourceScheduler}
	case SchedActive, SchedPending:
		return Decision{"pending", SourceScheduler}
	}
	// No new fact (scheduler Unknown/Gone, no wrapper) — leave the DB as is.
	return keep
}
