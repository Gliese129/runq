package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/store"
)

// fakeLane embeds the Backend interface (nil — methods panic if called,
// which no reconciler path does) and records lifecycle calls. When events
// is wired it appends "verb:id" markers so tests can assert ORDER (the
// review-#4 fence sequence), not just end state.
type fakeLane struct {
	backend.Backend
	name      string
	id        string
	closed    bool
	quiesced  bool
	resumed   bool
	started   bool
	drainFail bool // simulate a straggler submission that won't settle
	events    *[]string
}

func (f *fakeLane) rec(verb string) {
	if f.events != nil {
		*f.events = append(*f.events, verb+":"+f.id)
	}
}

func (f *fakeLane) Close() error          { f.closed = true; f.rec("close"); return nil }
func (f *fakeLane) Start(context.Context) { f.started = true; f.rec("start") }
func (f *fakeLane) Quiesce()              { f.quiesced = true; f.rec("quiesce") }
func (f *fakeLane) Resume()               { f.resumed = true; f.rec("resume") }
func (f *fakeLane) DrainSubmissions(context.Context) bool {
	f.rec("drain")
	return !f.drainFail
}

func writeCfg(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newReconcilerHarness builds a minimal client-shaped Daemon around a fake
// lane builder and a real (in-memory-store) MultiBackend. The returned
// failing set simulates TRANSIENT build failures (cluster unreachable):
// add a name to make its builds fail, remove it to "recover" — no config
// edit involved. scheduler: explode is the config-shaped failure hook.
func newReconcilerHarness(t *testing.T, dir string) (*Daemon, *[]string, map[string]bool) {
	t.Helper()
	t.Setenv("RUNQ_DATA_DIR", dir)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var built []string
	var events []string
	failing := map[string]bool{}
	seq := 0
	buildLane := func(tc config.TargetConfig, _ *config.GlobalConfig) (backend.Backend, error) {
		built = append(built, tc.Name)
		if failing[tc.Name] || tc.Scheduler == "explode" {
			return nil, os.ErrInvalid
		}
		seq++
		id := fmt.Sprintf("%s#%d", tc.Name, seq)
		events = append(events, "build:"+id)
		return &fakeLane{name: tc.Name, id: id, events: &events}, nil
	}

	lanes := map[string]backend.Backend{}
	targets := map[string]backend.Backend{}
	for _, tc := range cfg.ResolveTargets() {
		be, _ := buildLane(tc, cfg)
		lanes[tc.Name] = be
		targets[tc.Name] = be
	}
	built = nil // boot builds don't count in assertions

	multiBe, err := backend.NewMultiBackend(targets, st, cfg.ResolveDefaultTarget())
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		lanes:        lanes,
		multiBe:      multiBe,
		buildLane:    buildLane,
		lastTargets:  targetsByName(cfg),
		lastDefault:  cfg.ResolveDefaultTarget(),
		bootDataPath: cfg.DataPath,
		// Drain waits enabled (fakes drain instantly); grace 0 = sync close
		// so tests assert deterministically.
		laneDrainTimeout: time.Second,
	}
	return d, &built, failing
}

// harnessEvents digs the shared event log out of any live fakeLane.
func harnessEvents(d *Daemon) []string {
	for _, be := range d.lanes {
		if f, ok := be.(*fakeLane); ok && f.events != nil {
			return *f.events
		}
	}
	return nil
}

const baseCfg = `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: s
`

func TestReconcileAddChangeRemove(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, built, _ := newReconcilerHarness(t, dir)

	// ADD target b.
	writeCfg(t, dir, baseCfg+`  - name: b
    scheduler: pbs
    submit_template: s
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 1 || (*built)[0] != "b" {
		t.Fatalf("add: built = %v, want [b]", *built)
	}
	if _, err := d.multiBe.TargetFS("b"); err != nil {
		t.Fatalf("added target not routed: %v", err)
	}

	// CHANGE target b: old lane closed, new one routed.
	oldB := d.lanes["b"].(*fakeLane)
	writeCfg(t, dir, baseCfg+`  - name: b
    scheduler: pbs
    submit_template: CHANGED
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if !oldB.closed {
		t.Error("changed target's old lane not closed")
	}
	if d.lanes["b"].(*fakeLane) == oldB {
		t.Error("changed target still routed to the old lane")
	}

	// Reformat/comment only: NO rebuild (semantic no-op reaches the
	// reconciler only via API notify; it must diff to nothing).
	*built = nil
	writeCfg(t, dir, "# comment\n"+baseCfg+`  - name: b
    scheduler: pbs
    submit_template: CHANGED
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 0 {
		t.Fatalf("comment-only edit rebuilt lanes: %v", *built)
	}

	// REMOVE target b.
	curB := d.lanes["b"].(*fakeLane)
	writeCfg(t, dir, baseCfg)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if !curB.closed {
		t.Error("removed target's lane not closed")
	}
	if _, err := d.multiBe.TargetFS("b"); err == nil {
		t.Error("removed target still routed")
	}
	if _, ok := d.lanes["b"]; ok {
		t.Error("removed target still in lane map")
	}
}

func TestReconcileBrokenEditKeepsOldLane(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, built, _ := newReconcilerHarness(t, dir)
	oldA := d.lanes["a"].(*fakeLane)

	// A change whose build fails must keep the previous lane serving.
	writeCfg(t, dir, `default_target: a
targets:
  - name: a
    scheduler: explode
    submit_template: s
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if oldA.closed {
		t.Error("broken edit closed the working lane")
	}
	if d.lanes["a"].(*fakeLane) != oldA {
		t.Error("broken edit swapped routing away from the working lane")
	}

	// Review fix #2: the failure must NOT be recorded as applied — the
	// next pass (same file, no further edit) retries the rebuild.
	*built = nil
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 1 || (*built)[0] != "a" {
		t.Fatalf("no retry after failed rebuild: built = %v, want [a]", *built)
	}

	// Reverting to the ORIGINAL config heals cleanly: the record kept the
	// old target config, so the diff is empty and the working lane stays —
	// no pointless rebuild of a lane that already matches desired state.
	writeCfg(t, dir, baseCfg)
	*built = nil
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 0 {
		t.Fatalf("revert-to-original rebuilt the still-valid lane: %v", *built)
	}
	if d.lanes["a"].(*fakeLane) != oldA || oldA.closed {
		t.Error("revert-to-original disturbed the working lane")
	}

	// A DIFFERENT buildable edit after the failure swaps as usual.
	writeCfg(t, dir, `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: s2
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if d.lanes["a"].(*fakeLane) == oldA {
		t.Error("fixed edit did not swap to a fresh lane")
	}
	if !oldA.closed {
		t.Error("fixed edit left the superseded lane open")
	}
}

func TestReconcileAddFailureRetriesUntilRecovered(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, built, failing := newReconcilerHarness(t, dir)

	// Add a valid target while its cluster is temporarily unreachable.
	failing["b"] = true
	writeCfg(t, dir, baseCfg+`  - name: b
    scheduler: pbs
    submit_template: s
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.lanes["b"]; ok {
		t.Fatal("failed build produced a lane")
	}

	// Same file, next tick: retried (review fix #2 — a transient failure
	// must not strand the target until the next YAML edit).
	*built = nil
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 1 || (*built)[0] != "b" {
		t.Fatalf("no retry for failed add: built = %v, want [b]", *built)
	}

	// Cluster recovers — SAME file, no YAML edit: the next pass builds
	// and routes the lane.
	delete(failing, "b")
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.multiBe.TargetFS("b"); err != nil {
		t.Fatalf("recovered target not routed: %v", err)
	}
	if _, ok := d.lanes["b"]; !ok {
		t.Fatal("recovered target missing from lane map")
	}
}

// Review #4: same-name change must follow the fence sequence — cold-build
// the replacement, THEN quiesce+drain the old lane, THEN start the new one
// (restoreLane reads the settled DB), and close the old lane last.
func TestReconcileChangeFenceOrder(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, _, _ := newReconcilerHarness(t, dir)
	oldA := d.lanes["a"].(*fakeLane)

	writeCfg(t, dir, `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: s2
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}

	newA := d.lanes["a"].(*fakeLane)
	if newA == oldA {
		t.Fatal("routing did not swap to the replacement lane")
	}
	if !oldA.quiesced {
		t.Error("old lane was not fenced")
	}
	if !oldA.closed {
		t.Error("old lane not closed (grace 0 = synchronous)")
	}
	if !newA.started {
		t.Error("replacement lane not started")
	}

	ev := harnessEvents(d)
	idx := func(e string) int {
		for i, v := range ev {
			if v == e {
				return i
			}
		}
		t.Fatalf("event %s missing in %v", e, ev)
		return -1
	}
	build := idx("build:" + newA.id)
	quiesce := idx("quiesce:" + oldA.id)
	drain := idx("drain:" + oldA.id)
	start := idx("start:" + newA.id)
	closed := idx("close:" + oldA.id)
	if !(build < quiesce && quiesce < drain && drain < start && start < closed) {
		t.Fatalf("fence order wrong:\n%v\nwant build(new) < quiesce(old) < drain(old) < start(new) < close(old)", ev)
	}
}

// Review #4 follow-up: a drain timeout must ABORT the rotation — the old
// lane resumes serving, the never-started replacement is discarded, and
// the next pass (straggler settled) completes the swap.
func TestReconcileDrainTimeoutAbortsRotation(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, _, _ := newReconcilerHarness(t, dir)
	oldA := d.lanes["a"].(*fakeLane)
	oldA.drainFail = true // straggler submission won't settle in time

	writeCfg(t, dir, `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: s2
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}

	// Aborted: old lane still routed and serving, fence lifted.
	if d.lanes["a"].(*fakeLane) != oldA {
		t.Fatal("aborted rotation swapped routing anyway")
	}
	if oldA.closed {
		t.Error("aborted rotation closed the serving lane")
	}
	if !oldA.resumed {
		t.Error("aborted rotation did not lift the fence (Resume)")
	}
	// The replacement was discarded without ever starting.
	ev := harnessEvents(d)
	for _, e := range ev {
		if e == "start:a#2" {
			t.Fatalf("aborted rotation STARTED the replacement (double-submit risk): %v", ev)
		}
	}
	found := false
	for _, e := range ev {
		if e == "close:a#2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discarded replacement was not closed: %v", ev)
	}

	// Straggler settles → next pass completes the rotation, same file.
	oldA.drainFail = false
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	newA := d.lanes["a"].(*fakeLane)
	if newA == oldA {
		t.Fatal("retry pass did not swap to the replacement")
	}
	if !newA.started {
		t.Error("retry pass replacement not started")
	}
	if !oldA.closed {
		t.Error("retry pass left the superseded lane open")
	}
}

func TestReconcileDefaultTargetSwitch(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg+`  - name: b
    scheduler: pbs
    submit_template: s
`)
	d, _, _ := newReconcilerHarness(t, dir)

	writeCfg(t, dir, `default_target: b
targets:
  - name: a
    scheduler: slurm
    submit_template: s
  - name: b
    scheduler: pbs
    submit_template: s
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if got := d.multiBe.DefaultTargetName(); got != "b" {
		t.Fatalf("default target = %q, want b", got)
	}
}

func TestDiffTargets(t *testing.T) {
	mk := func(name, tmpl string) config.TargetConfig {
		return config.TargetConfig{Name: name, SubmitTemplate: tmpl}
	}
	old := map[string]config.TargetConfig{"a": mk("a", "1"), "b": mk("b", "1")}
	niu := map[string]config.TargetConfig{"a": mk("a", "1"), "b": mk("b", "2"), "c": mk("c", "1")}
	added, changed, removed := diffTargets(old, niu)
	sort.Strings(added)
	if len(added) != 1 || added[0] != "c" || len(changed) != 1 || changed[0] != "b" || len(removed) != 0 {
		t.Fatalf("diff = add %v change %v remove %v", added, changed, removed)
	}
	_, _, removed = diffTargets(niu, old)
	if len(removed) != 1 || removed[0] != "c" {
		t.Fatalf("reverse removed = %v", removed)
	}
}
