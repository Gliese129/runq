package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gliese129/runq-lab/internal/backend"
	"github.com/gliese129/runq-lab/internal/config"
	"github.com/gliese129/runq-lab/internal/store"
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
		ntc := ntc
		switch {
		case !ok:
			added = append(added, name)
		case !otc.SemanticEquals(&ntc):
			// SEMANTIC comparison (round 10): the diff must use the same
			// equivalence as the generation hash, or a representation-only
			// edit (nil map vs {}) rebuilds a lane under an unchanged
			// generation — active and retiring co-owning every row.
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
// tick, API write notify, connect); passes are serialized. It is a no-op in
// minimal assemblies that do not provide buildLane.
func (d *Daemon) ReconcileConfig(reason string) error {
	if d.buildLane == nil || d.shuttingDown.Load() {
		return nil
	}
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	if d.shuttingDown.Load() {
		return nil
	}

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
	// From the first observable scope change through the final registry/sweep
	// update, routed operations must see either the old generation topology or
	// the new one. Holding this writer lease also prevents a control operation
	// from continuing on a lane that is being retired beneath it.
	releaseRouting := d.multiBe.BeginRoutingUpdate()
	defer releaseRouting()
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
		// Persist first (failure = retry next pass, lane untouched).
		// Round 5 (user design): NOTHING is killed. The removed target's
		// lane keeps its full scheduling loop and runs everything already
		// accepted — pending included — under the config it was submitted
		// against, isolated, until the count reaches zero.
		retire, perr := d.persistRetirement(name, d.lastTargets[name], be, "removed")
		if perr != nil {
			record[name] = d.lastTargets[name]
			d.noteLaneBuildErr(name, fmt.Errorf("removal deferred: %w — retrying next pass", perr))
			continue
		}
		markRetiring(be)
		_ = d.StopRemoteForward(name)
		if retire {
			// Unroute + register-retiring in ONE registry transaction
			// (round 9 #1): no not-found window for concurrent task routing.
			d.multiBe.RetireTarget(name, laneGeneration(be), be)
			d.laneMu.Lock()
			delete(d.lanes, name)
			d.laneMu.Unlock()
			d.registerRetiring(name, laneGeneration(be), be)
		} else {
			d.multiBe.RemoveTarget(name)
			d.laneMu.Lock()
			delete(d.lanes, name)
			d.laneMu.Unlock()
			d.closeLaneDeferred(name, be)
		}
		d.clearLaneBuildErr(name)
		d.Logger.Info("lane removed", "target", name)
	}

	for _, name := range added {
		tc := newTargets[name]
		// A removed target re-added UNCHANGED (round 6 #4): its retiring
		// lane still exists with the same generation — promote it.
		if promoted, ok := d.takeRetiringLane(name, tc.SemanticGeneration()); ok {
			promoteActive(promoted)
			if d.Store != nil {
				mctx, mcancel := context.WithTimeout(context.Background(), 10*time.Second)
				if merr := d.Store.MarkGenerationDone(mctx, name, tc.SemanticGeneration()); merr != nil {
					d.Logger.Warn("mark promoted generation done failed", "target", name, "error", merr)
				}
				mcancel()
			}
			d.laneMu.Lock()
			d.lanes[name] = promoted
			d.laneMu.Unlock()
			d.multiBe.PromoteLane(name, tc.SemanticGeneration(), promoted, "", nil) // atomic (round 8 #2)
			d.clearLaneBuildErr(name)
			// Round 8 #4: a re-added remote_cli target gets its forward back.
			if tc.RemoteCLI && tc.SSH != nil {
				if ferr := d.startForwardFor(tc); ferr != nil {
					d.Logger.Warn("forward start failed", "target", name, "error", ferr)
				}
			}
			d.Logger.Info("retiring lane promoted back to active — target re-added unchanged", "target", name)
			continue
		}
		be, berr := d.buildLane(tc, cfg)
		if berr != nil {
			// The lane stays absent but VISIBLE: logged (deduped) and
			// surfacing on connect. NOT recorded — the next pass retries.
			delete(record, name)
			d.noteLaneBuildErr(name, berr)
			continue
		}
		if serr := startLane(be); serr != nil {
			closeLane(d, name, be)
			delete(record, name)
			d.noteLaneBuildErr(name, fmt.Errorf("lane recovery failed: %w", serr))
			continue
		}
		d.dropHistoricalLane(name, tc.SemanticGeneration())
		d.clearLaneBuildErr(name)
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
		// Belt-and-suspenders (round 10): if the semantic diff and the
		// generation hash ever disagree, an equal-generation "change" must
		// be a no-op — rebuilding would make two lanes co-own one
		// (target, generation).
		d.laneMu.Lock()
		curGen := laneGeneration(d.lanes[name])
		d.laneMu.Unlock()
		if g := tc.SemanticGeneration(); g != "" && g == curGen {
			d.Logger.Warn("changed target has an UNCHANGED generation — treating as no-op",
				"target", name, "generation", shortGen(g))
			continue
		}
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
		oldGen := laneGeneration(old)
		newGen := tc.SemanticGeneration()
		// 2. Persist the retirement record BEFORE anything observable
		//    changes. Failure = retry next pass.
		retire, perr := d.persistRetirement(name, d.lastTargets[name], old, "changed")
		if perr != nil {
			closeLane(d, name, newBe)
			record[name] = d.lastTargets[name]
			d.noteLaneBuildErr(name, fmt.Errorf("rotation deferred: %w — retrying next pass", perr))
			continue
		}
		// 3. Narrow the OLD lane's scope BEFORE the replacement exists
		//    (round 6 #2): once retiring, its sensors can never treat the
		//    incoming generation's rows as orphans.
		markRetiring(old)
		// 4. Generation loop (round 6 #4): if the new config's hash equals
		//    a still-retiring generation (A→B→A), PROMOTE that lane back to
		//    active instead of starting a same-hash twin.
		if promoted, ok := d.takeRetiringLane(name, newGen); ok {
			closeLane(d, name, newBe) // discard the cold-built twin
			promoteActive(promoted)
			if d.Store != nil {
				mctx, mcancel := context.WithTimeout(context.Background(), 10*time.Second)
				if merr := d.Store.MarkGenerationDone(mctx, name, newGen); merr != nil {
					// Record stays open; the rebuild guard closes it out as
					// an active-generation stale record — log, don't block.
					d.Logger.Warn("mark promoted generation done failed", "target", name, "error", merr)
				}
				mcancel()
			}
			// One transaction (round 9 #2): A up, B down — no window
			// where B's tasks route to A.
			d.multiBe.PromoteLane(name, newGen, promoted, oldGen, old)
			d.laneMu.Lock()
			d.lanes[name] = promoted
			d.laneMu.Unlock()
			d.Logger.Info("retiring lane promoted back to active — config returned to its generation",
				"target", name, "generation", shortGen(newGen))
		} else {
			// 5. Start the replacement, then swap active+retiring in ONE
			//    registry transaction (round 8 #2).
			if serr := startLane(newBe); serr != nil {
				closeLane(d, name, newBe)
				promoteActive(old)
				if retire && d.Store != nil {
					mctx, mcancel := context.WithTimeout(context.Background(), 10*time.Second)
					merr := d.Store.MarkGenerationDone(mctx, name, oldGen)
					mcancel()
					if merr != nil {
						d.Logger.Warn("rollback retirement record after failed replacement start", "target", name, "error", merr)
					}
				}
				record[name] = d.lastTargets[name]
				d.noteLaneBuildErr(name, fmt.Errorf("replacement recovery failed: %w", serr))
				continue
			}
			_ = d.StopRemoteForward(name)
			d.dropHistoricalLane(name, newGen)
			d.multiBe.RotateLane(name, newBe, oldGen, old)
			d.laneMu.Lock()
			d.lanes[name] = newBe
			d.laneMu.Unlock()
		}
		// Forward follows the ACTIVE lane, whichever object that now is
		// (round 8 #4 applies to the promotion path too).
		if tc.RemoteCLI && tc.SSH != nil {
			if ferr := d.startForwardFor(tc); ferr != nil {
				d.Logger.Warn("forward start failed", "target", name, "error", ferr)
			}
		}
		// 6. Register the superseded generation (ALWAYS when generated —
		//    round 6 #3: the sweep owns closing, after its two-zero
		//    confirmation; ungenerated fakes fall back to a plain close).
		if retire {
			d.registerRetiring(name, oldGen, old)
		} else {
			d.closeLaneDeferred(name, old)
		}
		// Cleared only after the FULL rotation succeeded — clearing at
		// build success would make an abort retry flip-flop the log.
		d.clearLaneBuildErr(name)
		d.Logger.Info("lane rebuilt from changed config", "target", name)
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
	d.sweepRetiringLocked()
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
func startLane(be backend.Backend) error {
	if s, ok := be.(interface{ Start(context.Context) error }); ok {
		return s.Start(context.Background())
	}
	if s, ok := be.(interface{ Start(context.Context) }); ok {
		s.Start(context.Background())
	}
	return nil
}

// persistRetirement records a superseding/removal BEFORE anything
// observable changes (round 6 #3: EVERY rotation registers — a zero
// count is not a lifecycle barrier; the sweep owns closing). Returns
// false for ungenerated lanes (test fakes): plain close instead.
func (d *Daemon) persistRetirement(name string, oldTC config.TargetConfig, be backend.Backend, reason string) (bool, error) {
	gen := laneGeneration(be)
	if gen == "" || d.Store == nil || d.multiBe == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
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
	d.Logger.Info("lane generation retiring", "target", name, "generation", shortGen(gen), "reason", reason)
	return true, nil
}

// takeRetiringLane pops a retiring lane whose generation matches (round 6
// #4: config changed BACK to a retiring generation — promote instead of
// building a same-hash twin that would co-own every row).
func (d *Daemon) takeRetiringLane(name, gen string) (backend.Backend, bool) {
	d.laneMu.Lock()
	be, ok := d.retiringLanes[name+"@"+gen]
	if ok {
		delete(d.retiringLanes, name+"@"+gen)
	}
	d.laneMu.Unlock()
	// Round 8 #3: a stale zero-confirmation must not carry into this
	// lane's NEXT retirement (it would close on the first sweep).
	delete(d.retireZeroSeen, name+"@"+gen)
	// multiBe's registry entry moves atomically in PromoteLane — never
	// here, so a concurrent reader cannot observe a gap.
	return be, ok
}

// promoteActive widens a promoted lane's scope back to active.
func promoteActive(be backend.Backend) {
	if p, ok := be.(interface{ PromoteActive() }); ok {
		p.PromoteActive()
	}
}

// registerRetiring flips the lane into retiring mode and registers it for
// generation-routed ops and the sweep. Persistence happened earlier
// (prepareRetirement) — this half cannot fail.
func (d *Daemon) registerRetiring(name, gen string, be backend.Backend) {
	// Daemon-side tracking ONLY (round 9): every multiBe registry move is
	// done atomically by the caller (RotateLane / RetireTarget /
	// PromoteLane) — doing it here again would reopen the window those
	// transactions close.
	d.laneMu.Lock()
	if d.retiringLanes == nil {
		d.retiringLanes = map[string]backend.Backend{}
	}
	d.retiringLanes[name+"@"+gen] = be
	d.laneMu.Unlock()
}

// SweepRetiringLanes closes every retiring lane whose unfinished-task
// count stayed at zero for TWO consecutive sweeps (round 5 #4: a submit
// still blocked in Prepare on the old pointer can write rows after a
// single zero reading — the confirmation window outlasts any bounded
// request). Level-triggered: after each reconcile pass + a 1min ticker.
func (d *Daemon) SweepRetiringLanes() {
	if d.shuttingDown.Load() {
		return
	}
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	if d.shuttingDown.Load() {
		return
	}
	releaseRouting := d.multiBe.BeginRoutingUpdate()
	defer releaseRouting()
	d.sweepRetiringLocked()
}

// sweepRetiringLocked is the body; the caller holds reconcileMu (round 6
// #5: the ticker sweep and reconcile-pass sweep otherwise race on
// retireZeroSeen and can double-Close a lane). reconcileMu is the ONE
// writer lock for every lane transition — rotation, promotion, sweep.
func (d *Daemon) sweepRetiringLocked() {
	if d.Store == nil {
		return
	}
	// Round 9 #3: a promotion whose MarkGenerationDone failed leaves an
	// open record for a generation that is ACTIVE again. Heal it every
	// sweep (level-triggered) instead of waiting for a restart.
	activeGens := map[string]string{} // target -> active generation
	d.laneMu.Lock()
	for name, be := range d.lanes {
		if g := laneGeneration(be); g != "" {
			activeGens[name] = g
		}
	}
	d.laneMu.Unlock()
	hctx, hcancel := context.WithTimeout(context.Background(), 10*time.Second)
	if open, oerr := d.Store.ListRetiringGenerations(hctx); oerr == nil {
		for _, g := range open {
			d.laneMu.Lock()
			_, registered := d.retiringLanes[g.Target+"@"+g.Generation]
			d.laneMu.Unlock()
			if !registered && activeGens[g.Target] == g.Generation {
				if merr := d.Store.MarkGenerationDone(hctx, g.Target, g.Generation); merr != nil {
					d.Logger.Warn("stale active-generation record heal failed — retrying next sweep",
						"target", g.Target, "error", merr)
				} else {
					d.Logger.Info("stale active-generation record closed",
						"target", g.Target, "generation", shortGen(g.Generation))
				}
			}
		}
	}
	hcancel()
	if d.retireZeroSeen == nil {
		d.retireZeroSeen = map[string]bool{}
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
			d.retireZeroSeen[key] = false
			cancel()
			continue
		}
		// SubmitJob and manual RetryTask share this HARD admission barrier:
		// intent/reset/publication may not have produced a countable row yet,
		// so the lane cannot close while either operation is in flight.
		if admission, ok2 := be.(interface{ HasInFlightAdmissions() bool }); ok2 && admission.HasInFlightAdmissions() {
			d.retireZeroSeen[key] = false
			cancel()
			continue
		}
		if !d.retireZeroSeen[key] {
			d.retireZeroSeen[key] = true // first zero: confirm next sweep
			cancel()
			continue
		}
		// Round 8 #5: the DB record closes FIRST — on failure the lane
		// stays registered and running; DB and runtime never split.
		if merr := d.Store.MarkGenerationDone(ctx, name, gen); merr != nil {
			d.Logger.Warn("mark generation done failed — lane kept, retrying next sweep",
				"target", name, "error", merr)
			d.retireZeroSeen[key] = false
			cancel()
			continue
		}
		delete(d.retireZeroSeen, key)
		if quiesceLaneForHistory(be) {
			d.multiBe.CompleteRetiringLane(name, gen, be)
			d.laneMu.Lock()
			delete(d.retiringLanes, key)
			if d.historicalLanes == nil {
				d.historicalLanes = map[string]backend.Backend{}
			}
			d.historicalLanes[key] = be
			d.laneMu.Unlock()
		} else {
			// Non-generation-aware test/minimal backends cannot be safely kept
			// as artifact readers after shutdown.
			d.multiBe.RemoveRetiringLane(name, gen)
			d.laneMu.Lock()
			delete(d.retiringLanes, key)
			d.laneMu.Unlock()
			closeLane(d, name, be)
		}
		cancel()
		d.Logger.Info("retired generation done — all its tasks reached terminal state",
			"target", name, "generation", shortGen(gen))
	}
}

// dropHistoricalLane closes a quiesced duplicate when its exact generation
// becomes active again; the active lane is now the correct artifact endpoint.
func (d *Daemon) dropHistoricalLane(name, generation string) {
	if generation == "" {
		return
	}
	be := d.multiBe.RemoveHistoricalLane(name, generation)
	if be == nil {
		return
	}
	d.laneMu.Lock()
	delete(d.historicalLanes, name+"@"+generation)
	d.laneMu.Unlock()
	closeLane(d, name, be)
}

// rebuildRetiringLanes restores retiring lanes after a daemon restart from
// their persisted config snapshots — long-running old-generation tasks
// keep being tracked on their original endpoints.
func (d *Daemon) rebuildRetiringLanes() error {
	if d.Store == nil || d.buildLane == nil || d.multiBe == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gens, err := d.Store.ListRetiringGenerations(ctx)
	if err != nil {
		return fmt.Errorf("list retiring generations: %w", err)
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		return fmt.Errorf("load config for retiring rebuild: %w", cerr)
	}
	var rebuildErrs []error
	var historical []store.TargetGenerationRow
	// Self-heal (round 5 #4): a generation marked done that still owns
	// unfinished rows (a straggler submit landed between the final sweep
	// and close) is REOPENED and its lane rebuilt.
	if all, aerr := d.Store.ListAllGenerations(ctx); aerr != nil {
		rebuildErrs = append(rebuildErrs, fmt.Errorf("list generation history: %w", aerr))
	} else {
		for _, g := range all {
			if g.DoneAt == nil {
				continue
			}
			n, nerr := d.Store.CountUnfinishedGenerationTasks(ctx, g.Target, g.Generation)
			if nerr != nil {
				rebuildErrs = append(rebuildErrs, fmt.Errorf("count unfinished tasks for %s@%s: %w", g.Target, shortGen(g.Generation), nerr))
				continue
			}
			if n == 0 {
				historical = append(historical, g)
				continue
			}
			g := g
			if uerr := d.Store.UpsertRetiredGeneration(ctx, &g); uerr != nil { // resets done_at
				rebuildErrs = append(rebuildErrs, fmt.Errorf("reopen generation %s@%s: %w", g.Target, shortGen(g.Generation), uerr))
				continue
			}
			gens = append(gens, g)
			d.Logger.Warn("done generation reopened — unfinished rows found after close",
				"target", g.Target, "generation", shortGen(g.Generation), "unfinished", n)
		}
	}
	cur := targetsByName(cfg)
	for _, g := range gens {
		d.laneMu.Lock()
		_, registered := d.retiringLanes[g.Target+"@"+g.Generation]
		d.laneMu.Unlock()
		if registered {
			continue
		}
		// A record matching the target's CURRENT generation is a stale
		// leftover from an aborted rotation (persist succeeded, a later
		// step failed, the lane stayed active) — close it out instead of
		// rebuilding a duplicate lane for the ACTIVE generation.
		if tc, ok := cur[g.Target]; ok && tc.SemanticGeneration() == g.Generation {
			if err := d.Store.MarkGenerationDone(ctx, g.Target, g.Generation); err != nil {
				rebuildErrs = append(rebuildErrs, fmt.Errorf("close stale active retirement %s@%s: %w", g.Target, shortGen(g.Generation), err))
				continue
			}
			d.Logger.Info("stale retirement record for the active generation closed",
				"target", g.Target, "generation", shortGen(g.Generation))
			continue
		}
		n, nerr := d.Store.CountUnfinishedGenerationTasks(ctx, g.Target, g.Generation)
		if nerr != nil {
			rebuildErrs = append(rebuildErrs, fmt.Errorf("count retiring tasks for %s@%s: %w", g.Target, shortGen(g.Generation), nerr))
			continue
		}
		if n == 0 {
			if err := d.Store.MarkGenerationDone(ctx, g.Target, g.Generation); err != nil {
				rebuildErrs = append(rebuildErrs, fmt.Errorf("close empty retirement %s@%s: %w", g.Target, shortGen(g.Generation), err))
			}
			continue
		}
		var tc config.TargetConfig
		if uerr := json.Unmarshal([]byte(g.ConfigJSON), &tc); uerr != nil {
			rebuildErrs = append(rebuildErrs, fmt.Errorf("decode retiring generation %s@%s: %w", g.Target, shortGen(g.Generation), uerr))
			continue
		}
		be, berr := d.buildLane(tc, cfg)
		if berr != nil {
			rebuildErrs = append(rebuildErrs, fmt.Errorf("build retiring generation %s@%s: %w", g.Target, shortGen(g.Generation), berr))
			continue
		}
		markRetiring(be) // BEFORE Start: restore must use the retiring filter
		if serr := startLane(be); serr != nil {
			closeLane(d, g.Target, be)
			rebuildErrs = append(rebuildErrs, fmt.Errorf("start retiring generation %s@%s: %w", g.Target, shortGen(g.Generation), serr))
			continue
		}

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
	// Re-read after the live pass because an empty open generation may have
	// been marked done above and should become artifact-routable immediately.
	if all, aerr := d.Store.ListAllGenerations(ctx); aerr == nil {
		historical = historical[:0]
		for _, g := range all {
			if g.DoneAt != nil {
				historical = append(historical, g)
			}
		}
	} else {
		rebuildErrs = append(rebuildErrs, fmt.Errorf("reload generation history: %w", aerr))
	}

	// Settled generations are rebuilt cold: no restore, scheduler, or sensor;
	// only their original filesystem endpoint remains available for terminal
	// logs and metric artifacts. Failure is non-fatal and routing then returns
	// an honest unavailable-generation error instead of using a newer host.
	if d.buildHistoricalLane != nil {
		for _, g := range historical {
			if tc, ok := cur[g.Target]; ok && tc.SemanticGeneration() == g.Generation {
				continue // the active lane is the same endpoint/config generation
			}
			key := g.Target + "@" + g.Generation
			d.laneMu.Lock()
			_, registered := d.historicalLanes[key]
			d.laneMu.Unlock()
			if registered {
				continue
			}
			var tc config.TargetConfig
			if uerr := json.Unmarshal([]byte(g.ConfigJSON), &tc); uerr != nil {
				d.Logger.Warn("historical generation snapshot invalid — artifacts unavailable",
					"target", g.Target, "generation", shortGen(g.Generation), "error", uerr)
				continue
			}
			be, berr := d.buildHistoricalLane(tc, cfg)
			if berr != nil {
				d.Logger.Warn("historical generation lane unavailable — artifacts will not fall through",
					"target", g.Target, "generation", shortGen(g.Generation), "error", berr)
				continue
			}
			markRetiring(be)
			d.multiBe.SetHistoricalLane(g.Target, g.Generation, be)
			d.laneMu.Lock()
			if d.historicalLanes == nil {
				d.historicalLanes = map[string]backend.Backend{}
			}
			d.historicalLanes[key] = be
			d.laneMu.Unlock()
			d.Logger.Info("historical generation artifact lane rebuilt",
				"target", g.Target, "generation", shortGen(g.Generation))
		}
	}
	return errors.Join(rebuildErrs...)
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

// quiesceLaneForHistory stops scheduling/sensors without closing the lane's
// filesystem. Only real generation-aware lanes expose this contract.
func quiesceLaneForHistory(be backend.Backend) bool {
	if q, ok := be.(interface{ QuiesceForHistory() }); ok {
		q.QuiesceForHistory()
		return true
	}
	return false
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
