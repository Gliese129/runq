package remote

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/gliese129/runq/internal/store"
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
	root := b.Cfg.Workspace
	if root == "" {
		return nil // no workspace root configured — cannot apply guardrail 2
	}
	if _, err := b.FS.Stat(root); err != nil {
		return nil // root unobservable/missing: config or mount problem, not orphans
	}

	rows, err := b.Store.ListTasks(ctx, store.TaskFilter{Target: b.Cfg.Name})
	if err != nil {
		return err
	}

	now := time.Now()
	for _, tk := range rows {
		if !isTerminal(tk.Status) || tk.TaskDir == "" {
			continue
		}
		_, serr := b.FS.Stat(tk.TaskDir)
		switch {
		case serr == nil:
			// Present: clear any stale mark and reset the strike counter.
			b.clearStrike(tk.ID)
			_ = b.Store.ClearTaskOrphaned(ctx, tk.ID)
		case errors.Is(serr, fs.ErrNotExist):
			if immediate || b.addStrike(tk.ID) >= 2 {
				if merr := b.Store.MarkTaskOrphaned(ctx, tk.ID, now); merr != nil && err == nil {
					err = merr
				}
			}
		default:
			// Transport or permission error: no fact learned. Leave the
			// strike counter untouched — do not accumulate toward a mark.
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
