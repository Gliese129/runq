package app

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
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
		// Unroute FIRST (no new requests can reach the lane), then fence
		// and drain, then close after grace (review #4).
		_ = d.StopRemoteForward(name)
		d.multiBe.RemoveTarget(name)
		d.laneMu.Lock()
		be := d.lanes[name]
		delete(d.lanes, name)
		d.laneMu.Unlock()
		d.quiesceLane(name, be)
		d.closeLaneDeferred(name, be)
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
		d.clearLaneBuildErr(name)
		d.laneMu.Lock()
		old := d.lanes[name]
		d.laneMu.Unlock()
		// 2. FENCE the old lane: no new dispatches, and in-flight
		//    submissions settle into the DB — so step 3's restoreLane reads
		//    settled rows. This closes the double-submit window (old lane
		//    launching a task the new lane also restored as pending).
		d.quiesceLane(name, old)
		// 3. Start the replacement (restoreLane reads the settled DB) and
		//    swap routing atomically — the target NAME never stops serving.
		startLane(newBe)
		_ = d.StopRemoteForward(name)
		d.multiBe.SetTarget(name, newBe)
		d.laneMu.Lock()
		d.lanes[name] = newBe
		d.laneMu.Unlock()
		// 4. The superseded lane closes after a grace period: reads/streams
		//    that grabbed the old pointer before the swap finish naturally;
		//    stragglers cut at the deadline get clean errors and their
		//    retries land on the new lane.
		d.closeLaneDeferred(name, old)
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

// quiesceLane fences a lane that is about to be superseded or removed
// (review #4): stop NEW dispatches, then wait (bounded) for in-flight
// submissions to settle into the DB. Lanes without the lifecycle (test
// fakes) are skipped. Drain timeout 0 means "fence but don't wait".
func (d *Daemon) quiesceLane(name string, be backend.Backend) {
	q, ok := be.(interface {
		Quiesce()
		DrainSubmissions(context.Context) bool
	})
	if !ok {
		return
	}
	q.Quiesce()
	if d.laneDrainTimeout <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.laneDrainTimeout)
	defer cancel()
	if !q.DrainSubmissions(ctx) {
		d.Logger.Warn("lane drain timed out — in-flight submissions will be interrupted and settle as unknown (reconcile heals)",
			"target", name, "timeout", d.laneDrainTimeout)
	}
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
