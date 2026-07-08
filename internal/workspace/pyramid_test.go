package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// writeMetrics writes a metrics.jsonl into a fresh task dir and returns it.
func writeMetrics(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(MetricsPath(dir), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// metricLine renders one metric event.
func metricLine(key string, value float64, step, ts int64) string {
	return fmt.Sprintf(`{"type":"metric","key":%q,"value":%g,"step":%d,"ts":%d}`, key, value, step, ts)
}

// genLinear emits n loss points value=i, step=i*stride, ts=1000+i.
func genLinear(key string, n int, stride int64) []string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, metricLine(key, float64(i), int64(i)*stride, 1000+int64(i)))
	}
	return lines
}

func mustBuild(t *testing.T, dir string) PyramidBuildStats {
	t.Helper()
	stats, err := BuildPyramid(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("BuildPyramid: %v", err)
	}
	return stats
}

// ── Build → Query round trip ────────────────────────────────────────────

func TestPyramidRoundTrip(t *testing.T) {
	const n = 1000
	dir := writeMetrics(t, genLinear("loss", n, 4))
	stats := mustBuild(t, dir)
	if stats.Points != n || stats.SkippedLines != 0 || stats.Keys != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	// Full range, generous budget → leaf-level buckets, everything adds up.
	buckets, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 10000)
	if err != nil {
		t.Fatalf("QueryPyramid: %v", err)
	}
	wantLeaves := (n + pyramidLeafWidth - 1) / pyramidLeafWidth
	if len(buckets) != wantLeaves {
		t.Fatalf("got %d buckets, want %d leaves", len(buckets), wantLeaves)
	}
	var count int64
	var sum float64
	for _, b := range buckets {
		count += b.Count
		sum += b.Sum
	}
	if count != n {
		t.Fatalf("Σcount = %d, want %d", count, n)
	}
	wantSum := float64(n-1) * float64(n) / 2 // Σ 0..n-1
	if math.Abs(sum-wantSum) > 1e-6 {
		t.Fatalf("Σsum = %g, want %g", sum, wantSum)
	}
	// Global extremes at the root view.
	root, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 1)
	if err != nil || len(root) != 1 {
		t.Fatalf("root query: %v (%d buckets)", err, len(root))
	}
	if root[0].Min != 0 || root[0].Max != float64(n-1) || root[0].Count != n {
		t.Fatalf("root = %+v", root[0])
	}
	// Steps propagated: stride 4 → last step (n-1)*4.
	if root[0].FirstStep != 0 || root[0].LastStep != int64(n-1)*4 {
		t.Fatalf("root steps = %d-%d", root[0].FirstStep, root[0].LastStep)
	}
}

// ── mergeToBudget: exact bucket counts, no ×fanout quantization ─────────

func TestPyramidQueryExactBudget(t *testing.T) {
	dir := writeMetrics(t, genLinear("loss", 4096, 1))
	mustBuild(t, dir)

	for _, budget := range []int{1, 7, 40, 100, 128, 2000} {
		buckets, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, budget)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if len(buckets) > budget {
			t.Fatalf("budget %d: got %d buckets", budget, len(buckets))
		}
		// With 128 leaves available, any budget ≤128 must be hit EXACTLY
		// (that's the anti-quantization property).
		if budget <= 128 && len(buckets) != budget {
			t.Fatalf("budget %d: got %d buckets, want exact", budget, len(buckets))
		}
		var count int64
		for _, b := range buckets {
			count += b.Count
		}
		if count != 4096 {
			t.Fatalf("budget %d: Σcount = %d", budget, count)
		}
	}
}

// ── std correctness through folds ───────────────────────────────────────

func TestPyramidStdExact(t *testing.T) {
	// Constant series → std 0 at every level; alternating ±1 → std 1.
	var lines []string
	for i := 0; i < 256; i++ {
		v := 1.0
		if i%2 == 1 {
			v = -1.0
		}
		lines = append(lines, metricLine("alt", v, int64(i), 1000+int64(i)))
		lines = append(lines, metricLine("flat", 5, int64(i), 1000+int64(i)))
	}
	dir := writeMetrics(t, lines)
	mustBuild(t, dir)

	for key, wantStd := range map[string]float64{"alt": 1, "flat": 0} {
		root, err := QueryPyramid(context.Background(), nil, dir, key, 0, 0, 1)
		if err != nil || len(root) != 1 {
			t.Fatalf("%s: %v", key, err)
		}
		if got := root[0].Std(); math.Abs(got-wantStd) > 1e-9 {
			t.Fatalf("%s: std = %g, want %g", key, got, wantStd)
		}
	}
}

// ── NaN accounting (SDK null convention) ────────────────────────────────

func TestPyramidNaNCount(t *testing.T) {
	lines := []string{
		metricLine("loss", 1, 0, 1000),
		`{"type":"metric","key":"loss","value":null,"step":1,"ts":1001}`,
		metricLine("loss", 3, 2, 1002),
	}
	dir := writeMetrics(t, lines)
	stats := mustBuild(t, dir)
	if stats.Points != 3 {
		t.Fatalf("points = %d", stats.Points)
	}
	root, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := root[0]
	if b.Count != 2 || b.NaNCount != 1 || b.Min != 1 || b.Max != 3 {
		t.Fatalf("bucket = %+v", b)
	}
}

// ── skipped-line accounting (bare NaN etc.) ─────────────────────────────

func TestPyramidSkippedLines(t *testing.T) {
	lines := []string{
		metricLine("loss", 1, 0, 1000),
		`{"type":"metric","key":"loss","value":NaN,"step":1,"ts":1001}`, // bare NaN: invalid JSON
		`total garbage`,
		`{"type":"checkpoint","path":"x","ts":1002}`,
		metricLine("loss", 2, 2, 1003),
	}
	dir := writeMetrics(t, lines)
	stats := mustBuild(t, dir)
	if stats.Points != 2 || stats.SkippedLines != 2 || stats.OtherEvents != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

// ── non-monotonic ts: extents use min/max, ts-range query still lands ───

func TestPyramidNonMonotonicTS(t *testing.T) {
	lines := []string{
		metricLine("loss", 1, 0, 1005), // late-stamped first point
		metricLine("loss", 2, 1, 1001),
		metricLine("loss", 3, 2, 1010),
		metricLine("loss", 4, 3, 1002),
	}
	dir := writeMetrics(t, lines)
	mustBuild(t, dir)
	root, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if root[0].FirstTS != 1001 || root[0].LastTS != 1010 {
		t.Fatalf("ts extent = %d-%d", root[0].FirstTS, root[0].LastTS)
	}
}

// ── ts-range query narrows to the right region ──────────────────────────

func TestPyramidRangeQuery(t *testing.T) {
	dir := writeMetrics(t, genLinear("loss", 4096, 1)) // ts = 1000..5095
	mustBuild(t, dir)

	buckets, err := QueryPyramid(context.Background(), nil, dir, "loss", 2000, 3000, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) == 0 {
		t.Fatal("empty range result")
	}
	// Every returned bucket overlaps [2000, 3000]; coverage is complete
	// (values 1000..2000 correspond to that ts window).
	for _, b := range buckets {
		if b.LastTS < 2000 || b.FirstTS > 3000 {
			t.Fatalf("bucket %d-%d outside range", b.FirstTS, b.LastTS)
		}
	}
	first, last := buckets[0], buckets[len(buckets)-1]
	if first.FirstTS > 2000 || last.LastTS < 3000 {
		t.Fatalf("range not covered: %d-%d .. %d-%d", first.FirstTS, first.LastTS, last.FirstTS, last.LastTS)
	}
}

// ── torn / invalid files → ErrPyramidNotBuilt ───────────────────────────

func TestPyramidTornFile(t *testing.T) {
	dir := writeMetrics(t, genLinear("loss", 512, 1))
	mustBuild(t, dir)

	full, err := os.ReadFile(PyramidPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":            {},
		"bad magic":        append([]byte("XXXX"), full[4:]...),
		"truncated header": full[:5],
		"truncated data":   full[:len(full)-pyramidRecordSize-7],
	}
	for name, corrupt := range cases {
		if err := os.WriteFile(PyramidPath(dir), corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 100); err == nil {
			t.Fatalf("%s: expected error, got none", name)
		}
	}
}

// ── partial trailing leaf + unknown key + inspect ───────────────────────

func TestPyramidTailAndInspect(t *testing.T) {
	dir := writeMetrics(t, genLinear("loss", 100, 1)) // 3 full leaves + 4-point tail
	mustBuild(t, dir)

	infos, err := InspectPyramid(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Key != "loss" || infos[0].PointCount != 100 {
		t.Fatalf("infos = %+v", infos)
	}
	leafCount := infos[0].LayerCounts[len(infos[0].LayerCounts)-1]
	if leafCount != 4 { // ceil(100/32)
		t.Fatalf("leaf count = %d", leafCount)
	}

	buckets, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := buckets[len(buckets)-1].Count; got != 4 {
		t.Fatalf("tail bucket count = %d, want 4", got)
	}

	if _, err := QueryPyramid(context.Background(), nil, dir, "nope", 0, 0, 100); err == nil {
		t.Fatal("unknown key: expected error")
	}
}

// ── no metrics → no file → ErrPyramidNotBuilt ───────────────────────────

func TestPyramidNoMetrics(t *testing.T) {
	dir := t.TempDir()
	if stats := mustBuild(t, dir); stats.Points != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, err := os.Stat(PyramidPath(dir)); !os.IsNotExist(err) {
		t.Fatal("pyramid file should not exist")
	}
	if _, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 10); err != ErrPyramidNotBuilt {
		t.Fatalf("err = %v", err)
	}
}

// ── raw ranges point back into metrics.jsonl ────────────────────────────

func TestPyramidRawRanges(t *testing.T) {
	dir := writeMetrics(t, genLinear("loss", 64, 1))
	mustBuild(t, dir)

	buckets, err := QueryPyramid(context.Background(), nil, dir, "loss", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(MetricsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range buckets {
		if b.RawStart < 0 || b.RawEnd > int64(len(raw)) || b.RawStart >= b.RawEnd {
			t.Fatalf("bucket %d: raw range %d-%d out of file bounds %d", i, b.RawStart, b.RawEnd, len(raw))
		}
		// The slice must contain exactly Count parseable metric lines.
		seen := 0
		for _, line := range strings.Split(strings.TrimRight(string(raw[b.RawStart:b.RawEnd]), "\n"), "\n") {
			var e struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(line), &e) == nil && e.Type == "metric" {
				seen++
			}
		}
		if int64(seen) != b.Count+b.NaNCount {
			t.Fatalf("bucket %d: raw slice has %d metric lines, bucket says %d", i, seen, b.Count+b.NaNCount)
		}
	}
}
