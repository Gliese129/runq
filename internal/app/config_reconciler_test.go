package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/store"
)

// fakeLane embeds the Backend interface (nil — methods panic if called,
// which no reconciler path does) and records lifecycle calls.
type fakeLane struct {
	backend.Backend
	name   string
	closed bool
}

func (f *fakeLane) Close() error { f.closed = true; return nil }

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
	failing := map[string]bool{}
	buildLane := func(tc config.TargetConfig, _ *config.GlobalConfig) (backend.Backend, error) {
		built = append(built, tc.Name)
		if failing[tc.Name] || tc.Scheduler == "explode" {
			return nil, os.ErrInvalid
		}
		return &fakeLane{name: tc.Name}, nil
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
	}
	return d, &built, failing
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
