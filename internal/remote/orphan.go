package remote

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"time"

	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
)

// DetectOrphans scans this target's terminal tasks for missing task dirs and
// marks them orphaned (reversible metadata; deletion is a separate, explicit
// `runq clean --orphan`). Observation over a network FS can lie, so three
// guardrails bound the blast radius:
//
//  1. Only fs.ErrNotExist counts. Transport errors (SSH down, timeout) are
//     "cannot observe", never "gone".
//  2. The workspace root itself must exist. A missing root means an unmounted
//     or misconfigured filesystem — that is ONE problem, not N orphans; the
//     scan aborts without marking anything.
//  3. Hysteresis (background mode): a task is marked only after two
//     consecutive scans observe it missing. immediate=true (interactive
//     clean) bypasses this — the user is looking at the list and confirming
//     each entry, which is a stronger safety layer than a second sample.
//
// Dirs observed present again get their mark cleared (restored from backup,
// or an earlier observation was wrong).
func (b *Backend) DetectOrphans(ctx context.Context, immediate bool) error {
	// Local lanes (LocalFS) observe their own machine: a stat is
	// authoritative, so the root/membership guardrails — built against
	// UNRELIABLE remote observation — don't apply. The circuit breaker
	// below still does (mis-mounted external disks look the same locally).
	_, localObservation := b.FS.(*rfs.LocalFS)

	root := b.Cfg.Workspace
	if !localObservation {
		if root == "" {
			return nil // no workspace root configured — cannot apply guardrail 2
		}
		if _, err := b.FS.Stat(root); err != nil {
			return nil // root unobservable/missing: config or mount problem, not orphans
		}
	}

	rows, err := b.Store.ListTasks(ctx, store.TaskFilter{Target: b.Cfg.Name, Scope: b.Scope})
	if err != nil {
		return err
	}

	// Two-phase: observe everything first, then decide. Marking inline would
	// commit to early observations before the circuit breaker below can see
	// the whole pattern.
	now := time.Now()
	var missing []store.TaskRow
	observed := 0
	rootPrefix := strings.TrimSuffix(root, "/") + "/"
	for _, tk := range rows {
		if !isTerminal(tk.Status) || tk.TaskDir == "" {
			continue
		}
		// Ownership validation (guardrail 2 proper, remote only): only dirs
		// UNDER the configured workspace are judged — the root guard above
		// then genuinely covers their common ancestor. A task dir outside
		// the workspace (pre-reconfiguration submissions, mis-pointed
		// config) has no verifiable relationship to the root we checked, so
		// it is never marked, no matter what a stat says. Local lanes skip
		// this: task dirs legitimately live under per-project roots.
		if !localObservation && !strings.HasPrefix(tk.TaskDir, rootPrefix) {
			continue
		}
		_, serr := b.FS.Stat(tk.TaskDir)
		switch {
		case serr == nil:
			// Present: clear any stale mark and reset the strike counter.
			observed++
			b.clearStrike(tk.ID)
			_ = b.Store.ClearTaskOrphaned(ctx, tk.ID)
		case errors.Is(serr, fs.ErrNotExist):
			observed++
			missing = append(missing, tk)
		default:
			// Transport or permission error: no fact learned. Leave the
			// strike counter untouched — do not accumulate toward a mark.
		}
	}

	// Circuit breaker (guardrail 2b): the root exists but EVERY observed dir
	// is missing. That pattern is far more likely a mis-pointed workspace or
	// half-mounted filesystem than N simultaneous deletions — the plain root
	// guard can be bypassed by `workspace:` pointing at some OTHER existing
	// path, and this catches exactly that. Refuse to mark anything.
	if len(missing) >= 3 && len(missing) == observed {
		opLog("ORPHAN SCAN ABORTED target=%s: all %d observed task dirs missing — suspect workspace/mount misconfiguration",
			b.Cfg.Name, observed)
		return err
	}

	for _, tk := range missing {
		if immediate || b.addStrike(tk.ID) >= 2 {
			if merr := b.Store.MarkTaskOrphaned(ctx, tk.ID, now); merr != nil && err == nil {
				err = merr
			}
		}
	}
	return err
}

// addStrike increments the consecutive-missing counter and returns the new
// value. clearStrike resets it. In-memory only: a daemon restart restarts
// the hysteresis, which errs on the safe side.
func (b *Backend) addStrike(taskID string) int {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if b.orphanStrikes == nil {
		b.orphanStrikes = make(map[string]int)
	}
	b.orphanStrikes[taskID]++
	return b.orphanStrikes[taskID]
}

func (b *Backend) clearStrike(taskID string) {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	delete(b.orphanStrikes, taskID)
}
