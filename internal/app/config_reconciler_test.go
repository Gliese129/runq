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
	name          string
	id            string
	gen           string // SemanticGeneration of the config this lane was built from
	closed        bool
	quiesced      bool
	resumed       bool
	started       bool
	retiring      bool
	drainFail     bool // simulate a straggler submission that won't settle
	forwardWired  bool
	removalReason string
	events        *[]string
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
func (f *fakeLane) Generation() string    { return f.gen }
func (f *fakeLane) MarkRetiring()         { f.retiring = true; f.rec("retire") }
func (f *fakeLane) DrainSubmissions(context.Context) bool {
	f.rec("drain")
	return !f.drainFail
}

// BeginRetirement records the forwarding wiring (RQ-75 round 4): resolve
// != nil = rotation (successor lookup), removalReason != "" = removal.
func (f *fakeLane) BeginRetirement(resolve func() (backend.Backend, bool), removalReason string) {
	f.quiesced = true
	f.retiring = true
	f.forwardWired = resolve != nil
	f.removalReason = removalReason
	f.rec("beginRetire")
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
		return &fakeLane{name: tc.Name, id: id, gen: tc.SemanticGeneration(), events: &events}, nil
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
		Store:        st,
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

// Round 4 (forwarding model): the rotation persists the retirement
// BEFORE the replacement starts, swaps, then wires forwarding — no
// fence, no drain. Order pinned via the event log.
func TestReconcileChangeForwardOrder(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, _, _ := newReconcilerHarness(t, dir)
	oldA := d.lanes["a"].(*fakeLane)
	seedLaneTask(t, d.Store, "t-x", "a", oldA.gen, "running", "e1") // forces retire

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
	if newA == oldA || !newA.started {
		t.Fatal("routing did not swap to a started replacement")
	}
	if !oldA.retiring || !oldA.forwardWired || oldA.removalReason != "" {
		t.Fatalf("old lane not wired for forwarding: %+v", oldA)
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
	if !(idx("build:"+newA.id) < idx("start:"+newA.id) && idx("start:"+newA.id) < idx("beginRetire:"+oldA.id)) {
		t.Fatalf("forward order wrong: %v", ev)
	}
}

// Round 4: a retirement-persist failure DEFERS the rotation (old lane
// untouched, no fence to lift) and the next pass completes it.
func TestReconcilePersistFailureDefersRotation(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, _, _ := newReconcilerHarness(t, dir)
	oldA := d.lanes["a"].(*fakeLane)
	seedLaneTask(t, d.Store, "t-p", "a", oldA.gen, "running", "e9")
	d.Store.Close() // every persistence call now fails

	writeCfg(t, dir, `default_target: a
targets:
  - name: a
    scheduler: slurm
    submit_template: s2
`)
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}
	if d.lanes["a"].(*fakeLane) != oldA || oldA.closed || oldA.retiring {
		t.Fatal("persist failure disturbed the serving lane")
	}
}
func seedLaneTask(t *testing.T, st *store.Store, id, target, gen, status, extID string) {
	t.Helper()
	jobID := "j-" + id
	_ = st.InsertJob(context.Background(), &store.JobRow{
		ID: jobID, ProjectName: "p", Status: "pending", TotalTasks: 1,
		CreatedAt: time.Now(), Target: target,
	})
	if err := st.InsertTask(context.Background(), &store.TaskRow{
		ID: id, JobID: jobID, ProjectName: "p", Command: "true",
		ParamsJSON: "{}", GPUsNeeded: 1, Status: status,
		EnqueuedAt: time.Now(), Target: target,
		TargetGeneration: gen, ExternalID: extID,
	}); err != nil {
		t.Fatal(err)
	}
}

// User-decided model: a superseded generation with in-flight tasks RETIRES
// (keeps tracking them) instead of closing; pending rows migrate to the
// new generation; the sweep closes the lane when its count hits zero.
func TestReconcileChangeRetiresLaneWithInflight(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg)
	d, _, _ := newReconcilerHarness(t, dir)
	oldA := d.lanes["a"].(*fakeLane)
	seedLaneTask(t, d.Store, "t-run", "a", oldA.gen, "running", "ext-1")
	seedLaneTask(t, d.Store, "t-pend", "a", oldA.gen, "pending", "")

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

	// Old lane retired, NOT closed — it still owns t-run.
	if oldA.closed {
		t.Fatal("lane with in-flight tasks was closed instead of retired")
	}
	if !oldA.retiring {
		t.Error("old lane not marked retiring")
	}
	if _, ok := d.retiringLanes["a@"+oldA.gen]; !ok {
		t.Error("old lane not registered as retiring")
	}
	gens, _ := d.Store.ListRetiringGenerations(context.Background())
	if len(gens) != 1 || gens[0].Generation != oldA.gen || gens[0].Reason != "changed" {
		t.Fatalf("retirement not persisted: %+v", gens)
	}

	// Round 4: pending rows are NOT bulk-restamped by the reconciler —
	// they move via the forwarding action (wired below), which restamps
	// per handoff on the receiving lane. Both rows keep the old stamp
	// here; the fake records that forwarding was wired.
	if !oldA.forwardWired {
		t.Fatal("rotation did not wire pending forwarding")
	}
	row, _ := d.Store.GetTask(context.Background(), "t-run")
	if row.TargetGeneration != oldA.gen {
		t.Fatalf("in-flight row was restamped: %q", row.TargetGeneration)
	}

	// Sweep with the task still running: lane stays.
	d.SweepRetiringLanes()
	if oldA.closed {
		t.Fatal("sweep closed a lane that still owns an unfinished task")
	}

	// Task reaches terminal; the pending row gets forwarded (simulated —
	// the fake lane has no scheduler; real lanes restamp on handoff) →
	// sweep retires the lane for good.
	if err := d.Store.UpdateTaskStatus(context.Background(), "t-run", "success", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Store.RestampTask(context.Background(), "t-pend", newA.gen); err != nil {
		t.Fatal(err)
	}
	d.SweepRetiringLanes()
	if !oldA.closed {
		t.Fatal("sweep did not close an emptied retiring lane")
	}
	if _, ok := d.retiringLanes["a@"+oldA.gen]; ok {
		t.Error("emptied lane still registered")
	}
	gens, _ = d.Store.ListRetiringGenerations(context.Background())
	if len(gens) != 0 {
		t.Fatalf("generation not marked done: %+v", gens)
	}
}

// Removed target: pending rows stop with a visible reason; the lane
// retires for its in-flight rows (reason 'removed').
func TestReconcileRemoveStopsPendingAndRetires(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, baseCfg+`  - name: b
    scheduler: pbs
    submit_template: s
`)
	d, _, _ := newReconcilerHarness(t, dir)
	oldB := d.lanes["b"].(*fakeLane)
	seedLaneTask(t, d.Store, "t-brun", "b", oldB.gen, "running", "ext-9")
	seedLaneTask(t, d.Store, "t-bpend", "b", oldB.gen, "pending", "")

	writeCfg(t, dir, baseCfg) // b removed
	if err := d.ReconcileConfig("test"); err != nil {
		t.Fatal(err)
	}

	// Round 4: pending settlement runs through the LANE's FinishTask
	// funnel (queue/DB/slots consistent), not a reconciler SQL sweep —
	// the fake records the wiring (no resolver, reason set).
	if oldB.removalReason == "" || oldB.forwardWired {
		t.Fatalf("removal retire action miswired: %+v", oldB)
	}
	row, _ := d.Store.GetTask(context.Background(), "t-brun")
	if row.Status != "running" {
		t.Fatalf("in-flight row of removed target was touched: %q", row.Status)
	}
	if oldB.closed {
		t.Fatal("removed lane with in-flight tasks was closed instead of retired")
	}
	gens, _ := d.Store.ListRetiringGenerations(context.Background())
	if len(gens) != 1 || gens[0].Reason != "removed" {
		t.Fatalf("removed-target retirement not persisted: %+v", gens)
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
