package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/store"
)

// ── RQ-75: config → lanes reconciler ────────────────────────────────────────
//
// config.yaml is the desired state; the running lanes are the observed
// state; this reconciler converges the second onto the first (the
// Kubernetes controller shape). It is level-triggered: every pass reloads
// the file and diffs per target, so a missed notification costs one watch
// interval, never correctness.
//
//	added target    → build lane (+ forward if remote_cli)
//	removed target  → unroute, close lane, stop forward
//	changed target  → build NEW lane first; only on success swap routing
//	                  and close the old one (a broken edit must not kill a
//	                  working lane); forward restarted alongside
//
// A lane rebuild ≈ a target-scoped mini-restart: remote lane state is
// DB-backed and restoreLane refills the queue, the exact recovery path a
// daemon restart uses. This is why "close old, build new" is safe and why
// errLaneRestartRequired could be deleted.

// diffTargets computes the per-target change set between two config
// snapshots. Pure function — the reconciler's decisions are testable
// without a daemon.
func diffTargets(old, new map[string]config.TargetConfig) (added, changed, removed []string) {
	for name, ntc := range new {
		otc, ok := old[name]
		switch {
		case !ok:
			added = append(added, name)
		case !reflect.DeepEqual(otc, ntc):
			changed = append(changed, name)
		}
	}
	for name := range old {
		if _, ok := new[name]; !ok {
			removed = append(removed, name)
		}
	}
	return added, changed, removed
}

// targetsByName indexes ResolveTargets output for diffing.
func targetsByName(cfg *config.GlobalConfig) map[string]config.TargetConfig {
	out := make(map[string]config.TargetConfig)
	for _, tc := range cfg.ResolveTargets() {
		out[tc.Name] = tc
	}
	return out
}

// ReconcileConfig runs one reconcile pass: reload config.yaml, diff against
// the observed lane set, apply. Safe to call from any goroutine (watcher
// tick, API write notify, connect); passes are serialized. No-op on runqd
// deployments (no buildLane).
func (d *Daemon) ReconcileConfig(reason string) error {
	if d.buildLane == nil {
		return nil
	}
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

	cfg, err := config.Load()
	if err != nil {
		// Parse failure: no fact learned, keep serving the last good state
		// (the gensync watcher already suppresses these, but API-triggered
		// passes reach here too).
		return fmt.Errorf("config reload: %w", err)
	}

	// Restart-bound keys: honesty over silent no-ops. data_path is a
	// storage root — hot-swapping it would strand in-flight task dirs.
	if d.bootDataPath != cfg.DataPath {
		d.Logger.Warn("data_path changed in config.yaml — this key is restart-bound; running lanes keep the boot value",
			"boot", d.bootDataPath, "file", cfg.DataPath)
	}

	newTargets := targetsByName(cfg)
	added, changed, removed := diffTargets(d.lastTargets, newTargets)
	if len(added)+len(changed)+len(removed) == 0 && cfg.ResolveDefaultTarget() == d.lastDefault {
		return nil
	}
	d.Logger.Info("config reconcile", "reason", reason,
		"added", added, "changed", changed, "removed", removed)

	// record is what this pass ACTUALLY applied (review fix #2): a failed
	// build must not be recorded as if it succeeded, or the level-triggered
	// retry dies — the next pass would see no diff and never rebuild, and a
	// transient failure (cluster briefly unreachable) would strand the
	// target laneless until the user edits YAML again. Failed adds are
	// dropped from the record; failed changes keep the OLD config, so every
	// subsequent pass retries. laneBuildErrs dedupes the retry logging.
	record := make(map[string]config.TargetConfig, len(newTargets))
	for k, v := range newTargets {
		record[k] = v
	}

	for _, name := range removed {
		d.laneMu.Lock()
		be := d.lanes[name]
		d.laneMu.Unlock()
		// Persist first (failure = retry next pass, lane untouched). No
		// fence (review round 4): pending rows — including any that race
		// in — are settled killed through the FinishTask funnel by the
		// retire action, keeping queue/DB/slots consistent by construction.
		retire, perr := d.prepareRetirement(name, d.lastTargets[name], be, "removed")
		if perr != nil {
			record[name] = d.lastTargets[name]
			d.noteLaneBuildErr(name, fmt.Errorf("removal deferred: %w — retrying next pass", perr))
			continue
		}
		_ = d.StopRemoteForward(name)
		d.multiBe.RemoveTarget(name)
		d.laneMu.Lock()
		delete(d.lanes, name)
		d.laneMu.Unlock()
		d.beginLaneRetirement(name, be,
			"target "+name+" was removed from config.yaml — pending task stopped before submission")
		if retire {
			d.registerRetiring(name, laneGeneration(be), be)
		} else {
			d.closeLaneDeferred(name, be)
		}
		d.clearLaneBuildErr(name)
		d.Logger.Info("lane removed", "target", name)
	}

	for _, name := range added {
		tc := newTargets[name]
		be, berr := d.buildLane(tc, cfg)
		if berr != nil {
			// The lane stays absent but VISIBLE: logged (deduped) and
			// surfacing on connect. NOT recorded — the next pass retries.
			delete(record, name)
			d.noteLaneBuildErr(name, berr)
			continue
		}
		d.clearLaneBuildErr(name)
		startLane(be)
		d.laneMu.Lock()
		d.lanes[name] = be
		d.laneMu.Unlock()
		d.multiBe.SetTarget(name, be)
		d.Logger.Info("lane added at runtime", "target", name)
		if tc.RemoteCLI && tc.SSH != nil {
			if ferr := d.startForwardFor(tc); ferr != nil {
				d.Logger.Warn("forward start failed", "target", name, "error", ferr)
			}
		}
	}

	// Same-name change = generation swap behind a continuous name (review
	// #4): the nginx-reload / K8s-termination shape, with StatefulSet-style
	// fencing for the stateful launch path.
	for _, name := range changed {
		tc := newTargets[name]
		// 1. COLD-build the replacement (dial + validate, NOT started): a
		//    broken edit must not disturb the working lane, and the fence
		//    below must not engage for a doomed swap.
		newBe, berr := d.buildLane(tc, cfg)
		if berr != nil {
			// Keep the old lane serving. The record keeps the OLD config so
			// the next pass still sees a diff and retries the rebuild.
			record[name] = d.lastTargets[name]
			d.noteLaneBuildErr(name, berr)
			continue
		}
		d.laneMu.Lock()
		old := d.lanes[name]
		d.laneMu.Unlock()
		// 2. Persist the retirement record BEFORE the replacement starts
		//    (its restore must SEE it, or it adopts the old generation).
		//    Failure = retry next pass; nothing observable changed. No
		//    fence, no drain (review round 4, user design): pending work
		//    is FORWARDED after the swap, so any racing submit or
		//    mid-flight dispatch converges instead of needing prevention.
		retire, perr := d.prepareRetirement(name, d.lastTargets[name], old, "changed")
		if perr != nil {
			closeLane(d, name, newBe)
			record[name] = d.lastTargets[name]
			d.noteLaneBuildErr(name, fmt.Errorf("rotation deferred: %w — retrying next pass", perr))
			continue
		}
		// 3. Start the replacement and swap routing atomically — the
		//    target NAME never stops serving.
		startLane(newBe)
		_ = d.StopRemoteForward(name)
		d.multiBe.SetTarget(name, newBe)
		d.laneMu.Lock()
		d.lanes[name] = newBe
		d.laneMu.Unlock()
		// 4. The superseded generation retires: scope narrows, dispatch
		//    stops, and every pending task (queued before OR straggling in
		//    after) is handed to the CURRENT active lane — resolved per
		//    handoff, so chained rotations always reach the newest one.
		d.beginLaneRetirement(name, old, "")
		if retire {
			d.registerRetiring(name, laneGeneration(old), old)
		} else {
			d.closeLaneDeferred(name, old)
		}
		// Cleared only after the FULL rotation succeeded — clearing at
		// build success would make an abort retry flip-flop the log.
		d.clearLaneBuildErr(name)
		d.Logger.Info("lane rebuilt from changed config", "target", name)
		if tc.RemoteCLI && tc.SSH != nil {
			if ferr := d.startForwardFor(tc); ferr != nil {
				d.Logger.Warn("forward start failed", "target", name, "error", ferr)
			}
		}
	}

	if nd := cfg.ResolveDefaultTarget(); nd != d.lastDefault {
		if serr := d.multiBe.SetDefaultTarget(nd); serr != nil {
			d.Logger.Warn("default target not switched", "wanted", nd, "error", serr)
		} else {
			d.Logger.Info("default target switched", "target", nd)
			d.lastDefault = nd
		}
	}

	d.lastTargets = record
	// Terminal transitions may already have emptied a retiring generation.
	d.SweepRetiringLanes()
	return nil
}

// noteLaneBuildErr logs a lane build failure ONCE per distinct error —
// the reconciler retries every pass (level-triggered), and an unreachable
// cluster must not turn the log into a 15s-interval siren. Called under
// reconcileMu.
func (d *Daemon) noteLaneBuildErr(name string, err error) {
	if d.laneBuildErrs == nil {
		d.laneBuildErrs = map[string]string{}
	}
	msg := err.Error()
	if d.laneBuildErrs[name] == msg {
		d.Logger.Debug("lane build retry failed (same error)", "target", name)
		return
	}
	d.laneBuildErrs[name] = msg
	d.Logger.Error("lane build failed — target not routed; retrying every reconcile pass",
		"target", name, "error", err)
}

// clearLaneBuildErr marks a previously failing lane as recovered. Called
// under reconcileMu.
func (d *Daemon) clearLaneBuildErr(name string) {
	if _, ok := d.laneBuildErrs[name]; ok {
		delete(d.laneBuildErrs, name)
		d.Logger.Info("lane build recovered", "target", name)
	}
}

// startLane starts a lane if it supports the lifecycle (SSHBackend does;
// test fakes may not).
func startLane(be backend.Backend) {
	if s, ok := be.(interface{ Start(context.Context) }); ok {
		s.Start(context.Background())
	}
}

// rotatableLane is the graceful-rotation lifecycle (SSHBackend implements
// it; test fakes may).
type rotatableLane interface {
	Quiesce()
	Resume()
	DrainSubmissions(context.Context) bool
}

// quiesceLane fences a lane that is about to be superseded or removed
// (review #4): stop NEW dispatches, then wait (bounded) for in-flight
// submissions to settle into the DB. Returns false when the drain timed
// out — the caller decides (a supersede ABORTS; a removal proceeds).
// Lanes without the lifecycle (test fakes) report drained. Drain timeout
// 0 means "fence but don't wait".
func (d *Daemon) quiesceLane(name string, be backend.Backend) bool {
	q, ok := be.(rotatableLane)
	if !ok {
		return true
	}
	q.Quiesce()
	if d.laneDrainTimeout <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.laneDrainTimeout)
	defer cancel()
	return q.DrainSubmissions(ctx)
}

// resumeLane lifts the fence after an aborted rotation.
func resumeLane(be backend.Backend) {
	if q, ok := be.(rotatableLane); ok {
		q.Resume()
	}
}

// prepareRetirement runs the PERSISTENCE half of a rotation, BEFORE the
// replacement starts (review #2/#4): count the old generation's unfinished
// tasks, persist the retirement record when needed, and migrate pending
// rows to the new generation. Every failure is returned — the caller
// aborts the rotation (nothing observable has changed yet: writes are
// idempotent and re-run next pass). Returns whether the lane must retire.
func (d *Daemon) prepareRetirement(name string, oldTC config.TargetConfig, be backend.Backend, reason string) (bool, error) {
	gen := laneGeneration(be)
	if gen == "" || d.Store == nil || d.multiBe == nil {
		return false, nil // ungenerated lane (test fake without gen): plain close
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	n, err := d.Store.CountUnfinishedGenerationTasks(ctx, name, gen)
	if err != nil {
		return false, fmt.Errorf("unfinished-count query failed: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	snap, merr := json.Marshal(oldTC)
	if merr != nil {
		return false, fmt.Errorf("target config snapshot failed: %w", merr)
	}
	if uerr := d.Store.UpsertRetiredGeneration(ctx, &store.TargetGenerationRow{
		Target: name, Generation: gen, ConfigJSON: string(snap),
		Reason: reason, RetiredAt: time.Now().Unix(),
	}); uerr != nil {
		return false, fmt.Errorf("persist retiring generation failed: %w", uerr)
	}
	d.Logger.Info("lane retiring — tracking its remaining tasks to completion",
		"target", name, "generation", shortGen(gen), "unfinished", n, "reason", reason)
	return true, nil
}

// beginLaneRetirement wires a lane's retire action (RQ-75 forwarding):
// resolve the CURRENT active lane per handoff (chained rotations reach
// the newest generation); removalReason set = settle instead of forward.
// No-op for lanes without the capability (test fakes fall back to their
// recorded Quiesce via markRetiring later).
func (d *Daemon) beginLaneRetirement(name string, be backend.Backend, removalReason string) {
	br, ok := be.(interface {
		BeginRetirement(func() (backend.Backend, bool), string)
	})
	if !ok {
		return
	}
	var resolve func() (backend.Backend, bool)
	if removalReason == "" {
		resolve = func() (backend.Backend, bool) { return d.multiBe.ActiveLane(name) }
	}
	br.BeginRetirement(resolve, removalReason)
}

// registerRetiring flips the lane into retiring mode and registers it for
// generation-routed ops and the sweep. Persistence happened earlier
// (prepareRetirement) — this half cannot fail.
func (d *Daemon) registerRetiring(name, gen string, be backend.Backend) {
	markRetiring(be)
	d.multiBe.SetRetiringLane(name, gen, be)
	d.laneMu.Lock()
	if d.retiringLanes == nil {
		d.retiringLanes = map[string]backend.Backend{}
	}
	d.retiringLanes[name+"@"+gen] = be
	d.laneMu.Unlock()
}

// SweepRetiringLanes closes every retiring lane whose unfinished-task
// count reached zero and marks its generation done. Level-triggered:
// called after each reconcile pass and on a periodic tick.
func (d *Daemon) SweepRetiringLanes() {
	if d.Store == nil {
		return
	}
	d.laneMu.Lock()
	snapshot := make(map[string]backend.Backend, len(d.retiringLanes))
	for k, v := range d.retiringLanes {
		snapshot[k] = v
	}
	d.laneMu.Unlock()
	for key, be := range snapshot {
		name, gen, ok := strings.Cut(key, "@")
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		n, err := d.Store.CountUnfinishedGenerationTasks(ctx, name, gen)
		if err != nil || n > 0 {
			cancel()
			continue
		}
		d.multiBe.RemoveRetiringLane(name, gen)
		d.laneMu.Lock()
		delete(d.retiringLanes, key)
		d.laneMu.Unlock()
		closeLane(d, name, be)
		if merr := d.Store.MarkGenerationDone(ctx, name, gen); merr != nil {
			d.Logger.Warn("mark generation done failed", "target", name, "error", merr)
		}
		cancel()
		d.Logger.Info("retired generation done — all its tasks reached terminal state",
			"target", name, "generation", shortGen(gen))
	}
}

// rebuildRetiringLanes restores retiring lanes after a daemon restart from
// their persisted config snapshots — long-running old-generation tasks
// keep being tracked on their original endpoints.
func (d *Daemon) rebuildRetiringLanes() {
	if d.Store == nil || d.buildLane == nil || d.multiBe == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gens, err := d.Store.ListRetiringGenerations(ctx)
	if err != nil {
		d.Logger.Warn("list retiring generations failed", "error", err)
		return
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		d.Logger.Warn("config load for retiring rebuild failed", "error", cerr)
		return
	}
	cur := targetsByName(cfg)
	for _, g := range gens {
		// A record matching the target's CURRENT generation is a stale
		// leftover from an aborted rotation (persist succeeded, a later
		// step failed, the lane stayed active) — close it out instead of
		// rebuilding a duplicate lane for the ACTIVE generation.
		if tc, ok := cur[g.Target]; ok && tc.SemanticGeneration() == g.Generation {
			_ = d.Store.MarkGenerationDone(ctx, g.Target, g.Generation)
			d.Logger.Info("stale retirement record for the active generation closed",
				"target", g.Target, "generation", shortGen(g.Generation))
			continue
		}
		if n, nerr := d.Store.CountUnfinishedGenerationTasks(ctx, g.Target, g.Generation); nerr == nil && n == 0 {
			_ = d.Store.MarkGenerationDone(ctx, g.Target, g.Generation)
			continue
		}
		var tc config.TargetConfig
		if uerr := json.Unmarshal([]byte(g.ConfigJSON), &tc); uerr != nil {
			d.Logger.Error("retiring generation snapshot unreadable — its tasks stay visible but untracked",
				"target", g.Target, "generation", shortGen(g.Generation), "error", uerr)
			continue
		}
		be, berr := d.buildLane(tc, cfg)
		if berr != nil {
			d.Logger.Error("retiring lane rebuild failed — its tasks stay visible but untracked until the endpoint returns",
				"target", g.Target, "generation", shortGen(g.Generation), "error", berr)
			continue
		}
		markRetiring(be) // BEFORE Start: restore must use the retiring filter
		startLane(be)
		d.multiBe.SetRetiringLane(g.Target, g.Generation, be)
		d.laneMu.Lock()
		if d.retiringLanes == nil {
			d.retiringLanes = map[string]backend.Backend{}
		}
		d.retiringLanes[g.Target+"@"+g.Generation] = be
		d.laneMu.Unlock()
		d.Logger.Info("retiring lane rebuilt after restart",
			"target", g.Target, "generation", shortGen(g.Generation), "reason", g.Reason)
	}
}

// laneGeneration reads a lane's generation stamp (SSHBackend has one;
// fakes may).
func laneGeneration(be backend.Backend) string {
	if g, ok := be.(interface{ Generation() string }); ok {
		return g.Generation()
	}
	return ""
}

// markRetiring flips a lane into retiring mode (quiesced + restore
// filters to its own generation).
func markRetiring(be backend.Backend) {
	if m, ok := be.(interface{ MarkRetiring() }); ok {
		m.MarkRetiring()
	}
}

// shortGen abbreviates a generation hash for logs.
func shortGen(gen string) string {
	if len(gen) > 12 {
		return gen[:12]
	}
	return gen
}

// closeLaneDeferred closes a superseded/removed lane after laneCloseGrace,
// so reads and log streams that picked up the old pointer before the
// routing swap can finish. Zero grace closes synchronously (tests).
func (d *Daemon) closeLaneDeferred(name string, be backend.Backend) {
	if be == nil {
		return
	}
	if d.laneCloseGrace <= 0 {
		closeLane(d, name, be)
		return
	}
	time.AfterFunc(d.laneCloseGrace, func() {
		d.Logger.Info("closing superseded lane after grace period", "target", name)
		closeLane(d, name, be)
	})
}

// closeLane closes a lane if it supports the lifecycle.
func closeLane(d *Daemon, name string, be backend.Backend) {
	if be == nil {
		return
	}
	if c, ok := be.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			d.Logger.Warn("lane close failed", "target", name, "error", err)
		}
	}
}
