// pyramid.go — the on-target multi-resolution metrics index
// (metrics.pyr): an implicit segment-tree laid out as mipmap levels, built
// NEXT TO metrics.jsonl on the machine that wrote it — Build runs on the
// COMPUTE NODE from run.sh, right before the done marker (`"$RUNQ_BIN"
// metrics-index build ... || true`). Zero transfer, pure local IO; the
// login node never carries this work.
//
// Why a pyramid and not a pointer tree: over SSH the cost model is round
// trips, not node count. A chart query at any resolution is a handful of
// CONTIGUOUS level slices → one seek + one ranged read each (the
// logfile.Reader pattern). Per-node children offsets are pure arithmetic
// (fixed-size records × fixed fanout); the offsets worth STORING point
// into the RAW file — zoom past the leaf level = one ranged read of
// metrics.jsonl.
//
// ── File format v1 (all integers little-endian) ─────────────────────────
//
//	header:
//	  magic "RQPY" | version u8 | key_count u16
//	  per key (sorted by name, reproducible builds):
//	    key_len u16 | key bytes | point_count u64 |
//	    layer_count u8 | layer_start []u64   (absolute byte offsets,
//	                                          COARSEST level first)
//	data:
//	  per key, per layer (coarsest first), fixed 96B records:
//	    min f64 | max f64 | sum f64 | sum_sq f64 |
//	    count u64 | nan_count u64 |
//	    first_ts i64 | last_ts i64 | first_step i64 | last_step i64 |
//	    raw_start i64 | raw_end i64
//
// pyramidLeafWidth = 32 raw points per leaf bucket; pyramidFanout = 4 per level up.
// Buckets are POINT-INDEX aligned (not time): addressing is exact
// arithmetic; time-range queries walk down the levels narrowing the
// span (first_ts/last_ts min/max per bucket — ts need not be strictly
// monotonic). Build writes metrics.pyr.tmp then renames (atomic where
// the FS supports it).

package workspace

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sort"

	"github.com/gliese129/runq-lab/internal/rfs"
)

const (
	pyramidMagic     = "RQPY"
	pyramidVersion   = 1
	pyramidLeafWidth = 32 // raw points per leaf bucket
	pyramidFanout    = 4  // buckets merged per level up

	// pyramidRecordSize is the fixed on-disk bucket size (12 fields × 8 bytes).
	pyramidRecordSize = 96

	// pyramidMaxBuildBytes caps the in-memory bucket set (~26:1 compression over
	// raw, so this admits ~13GB of raw jsonl). Beyond it something is
	// pathological; fail and let run.sh's `|| true` absorb it.
	pyramidMaxBuildBytes = 512 << 20
)

// PyramidBucket is one aggregated node. Every field is ASSOCIATIVE — the
// format's invariant: any regrouping of buckets yields exact values, which
// is what makes build-fold and query-remerge share one operator.
//
//	Sum/Count → avg;  SumSq → exact std (√(SumSq/Count − avg²));
//	Min/Max → band;  NaNCount → divergence marking (SDK writes null for
//	non-finite values — bare NaN isn't valid JSON);
//	First/LastStep → the step axis (ML charts' default x; -1 = SDK logged
//	no step);  RawStart/RawEnd → the bucket's region of metrics.jsonl
//	(other keys' lines interleave — readers filter after the ranged read).
//
// argmin/argmax positions are deliberately NOT stored: descending into the
// child whose max equals the parent's max locates the best point in
// O(log n) reads — storing it would be redundant. Medians/quantiles are
// deliberately absent: not associative (would need sketches and variable
// records, killing offset arithmetic).
type PyramidBucket struct {
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Sum       float64 `json:"sum"`
	SumSq     float64 `json:"sum_sq"`
	Count     int64   `json:"count"`
	NaNCount  int64   `json:"nan_count"` // null-valued metric events (non-finite by SDK convention)
	FirstTS   int64   `json:"first_ts"`
	LastTS    int64   `json:"last_ts"`
	FirstStep int64   `json:"first_step"` // -1 = no step logged
	LastStep  int64   `json:"last_step"`  // -1 = no step logged
	RawStart  int64   `json:"raw_start"`
	RawEnd    int64   `json:"raw_end"`
}

// Avg returns the bucket mean (0 for empty/NaN-only buckets).
func (b PyramidBucket) Avg() float64 {
	if b.Count == 0 {
		return 0
	}
	return b.Sum / float64(b.Count)
}

// Std returns the exact population standard deviation — SumSq is carried
// through every fold, so this is precise at any zoom level, not an
// approximation. (max guards tiny negative values from float rounding.)
func (b PyramidBucket) Std() float64 {
	if b.Count == 0 {
		return 0
	}
	avg := b.Avg()
	return math.Sqrt(math.Max(0, b.SumSq/float64(b.Count)-avg*avg))
}

// ErrPyramidNotBuilt reports that the task dir has no (valid) pyramid — callers
// fall back to the raw tail window.
var ErrPyramidNotBuilt = errors.New("pyramid: index not built")

// ── Build ────────────────────────────────────────────────────────────────

// PyramidBuildStats accounts for every input line — dropped lines must be
// OBSERVABLE, never silent: a parse-skipped line (bare NaN from Python's
// json.dumps, truncated tail, junk) shows up in SkippedLines instead of
// quietly vanishing from the chart.
type PyramidBuildStats struct {
	Keys         int   `json:"keys"`
	Points       int64 `json:"points"`        // metric events folded (incl. NaN-counted)
	OtherEvents  int64 `json:"other_events"`  // valid JSON, non-metric type (checkpoint…)
	SkippedLines int64 `json:"skipped_lines"` // unparseable lines
}

// Build reads <taskDir>/metrics.jsonl once, streaming, and writes
// <taskDir>/metrics.pyr. Meant to run WHERE THE FILE LIVES (compute node;
// fsys nil = local FS): building over SSHFS would pull the whole raw file
// — exactly what the pyramid exists to avoid.
//
// Memory = leaves ≈ raw/26 (each 32 raw points fold into one 96B bucket
// while streaming); upper levels add a geometric 1/3. No metric events →
// no file (Query's ErrPyramidNotBuilt covers it).
func BuildPyramid(ctx context.Context, fsys rfs.FS, taskDir string) (PyramidBuildStats, error) {
	var stats PyramidBuildStats
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	rawPath := MetricsPath(taskDir)
	f, err := fsys.Open(rawPath)
	if errors.Is(err, fs.ErrNotExist) {
		return stats, nil // no SDK output: nothing to index, not an error
	}
	if err != nil {
		return stats, err
	}
	defer f.Close()

	// ── pass 1: stream raw, fold into leaf buckets per key ──
	type keyBuild struct {
		leaves []PyramidBucket
		cur    PyramidBucket // accumulator for the unfinished leaf
		curN   int64
	}
	keys := map[string]*keyBuild{}
	memBytes := 0

	br := bufio.NewReaderSize(f, 256*1024)
	var offset int64 // byte position of the current line's start
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		raw, rerr := br.ReadBytes('\n')
		lineStart := offset
		offset += int64(len(raw))
		if len(raw) > 1 {
			var e struct {
				Type  string   `json:"type"`
				Key   string   `json:"key"`
				Value *float64 `json:"value"` // null = non-finite (SDK convention) → NaNCount
				Step  *int64   `json:"step"`
				TS    int64    `json:"ts"`
			}
			switch {
			case json.Unmarshal(raw, &e) != nil:
				stats.SkippedLines++ // unparseable (bare NaN, truncation, junk)
			case e.Type != "metric" || e.Key == "":
				stats.OtherEvents++ // checkpoint etc. — not chart data
			default:
				stats.Points++
				kb, ok := keys[e.Key]
				if !ok {
					kb = &keyBuild{}
					keys[e.Key] = kb
					memBytes += len(e.Key) + pyramidRecordSize
				}
				foldPoint(&kb.cur, &kb.curN, e.Value, e.Step, e.TS, lineStart, offset)
				if kb.curN == pyramidLeafWidth {
					kb.leaves = append(kb.leaves, kb.cur)
					kb.cur, kb.curN = PyramidBucket{}, 0
					memBytes += pyramidRecordSize
					if memBytes > pyramidMaxBuildBytes {
						return stats, fmt.Errorf("pyramid: bucket set exceeds %dMB, refusing (raw pathologically large?)", pyramidMaxBuildBytes>>20)
					}
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				return stats, rerr
			}
			break
		}
	}
	// Seal partial trailing leaves.
	names := make([]string, 0, len(keys))
	for name, kb := range keys {
		if kb.curN > 0 {
			kb.leaves = append(kb.leaves, kb.cur)
		}
		if len(kb.leaves) > 0 {
			names = append(names, name)
		}
	}
	stats.Keys = len(names)
	if len(names) == 0 {
		return stats, nil // metric-free file: build nothing
	}
	sort.Strings(names)

	// ── pass 2: in-memory fold to upper levels, then one sequential write ──
	type keyLayers struct {
		pointCount int64
		levels     [][]PyramidBucket // [0] = coarsest ... [last] = leaves
	}
	built := make([]keyLayers, len(names))
	for i, name := range names {
		kb := keys[name]
		levels := [][]PyramidBucket{kb.leaves}
		for len(levels[len(levels)-1]) > 1 {
			levels = append(levels, foldLevel(levels[len(levels)-1]))
		}
		// Reverse: file stores coarsest first (descent reads narrow → wide).
		for l, r := 0, len(levels)-1; l < r; l, r = l+1, r-1 {
			levels[l], levels[r] = levels[r], levels[l]
		}
		var pc int64
		for _, lf := range kb.leaves {
			pc += lf.Count + lf.NaNCount
		}
		built[i] = keyLayers{pointCount: pc, levels: levels}
	}

	// Header size: magic+version+key_count, then per-key fixed parts.
	headerSize := 4 + 1 + 2
	for i, name := range names {
		headerSize += 2 + len(name) + 8 + 1 + 8*len(built[i].levels)
	}
	// Assign absolute layer offsets.
	dataOff := int64(headerSize)
	layerStarts := make([][]uint64, len(names))
	for i := range names {
		starts := make([]uint64, len(built[i].levels))
		for li, level := range built[i].levels {
			starts[li] = uint64(dataOff)
			dataOff += int64(len(level) * pyramidRecordSize)
		}
		layerStarts[i] = starts
	}

	buf := make([]byte, 0, dataOff)
	w := newLEWriter(buf)
	w.bytes([]byte(pyramidMagic))
	w.u8(pyramidVersion)
	w.u16(uint16(len(names)))
	for i, name := range names {
		w.u16(uint16(len(name)))
		w.bytes([]byte(name))
		w.u64(uint64(built[i].pointCount))
		w.u8(uint8(len(built[i].levels)))
		for _, s := range layerStarts[i] {
			w.u64(s)
		}
	}
	for i := range names {
		for _, level := range built[i].levels {
			for _, b := range level {
				w.bucket(b)
			}
		}
	}
	if int64(len(w.buf)) != dataOff {
		return stats, fmt.Errorf("pyramid: layout mismatch (wrote %d, expected %d)", len(w.buf), dataOff)
	}

	tmp := PyramidPath(taskDir) + ".tmp"
	final := PyramidPath(taskDir)
	if err := fsys.WriteFile(tmp, w.buf, 0o644); err != nil {
		return stats, err
	}
	if r, ok := fsys.(interface {
		Rename(oldPath, newPath string) error
	}); ok {
		return stats, r.Rename(tmp, final)
	}
	// No atomic rename on this FS: direct write; Query validates magic +
	// bounds and treats torn files as ErrPyramidNotBuilt.
	return stats, fsys.WriteFile(final, w.buf, 0o644)
}

// foldPoint folds one raw metric event into the open leaf accumulator.
// A nil value (SDK writes null for non-finite) counts toward NaNCount and
// the bucket's extent (ts/step/raw range) but not toward the statistics.
func foldPoint(b *PyramidBucket, n *int64, v *float64, step *int64, ts, lineStart, lineEnd int64) {
	if *n == 0 {
		*b = PyramidBucket{
			Min: math.Inf(1), Max: math.Inf(-1),
			FirstTS: ts, LastTS: ts,
			FirstStep: -1, LastStep: -1,
			RawStart: lineStart, RawEnd: lineEnd,
		}
	}
	*n++
	if ts < b.FirstTS {
		b.FirstTS = ts
	}
	if ts > b.LastTS {
		b.LastTS = ts
	}
	if step != nil {
		if b.FirstStep == -1 || *step < b.FirstStep {
			b.FirstStep = *step
		}
		if *step > b.LastStep {
			b.LastStep = *step
		}
	}
	b.RawEnd = lineEnd
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		b.NaNCount++
		return
	}
	b.Min = math.Min(b.Min, *v)
	b.Max = math.Max(b.Max, *v)
	b.Sum += *v
	b.SumSq += *v * *v
	b.Count++
}

// foldLevel merges groups of pyramidFanout buckets (last group may be short).
func foldLevel(level []PyramidBucket) []PyramidBucket {
	out := make([]PyramidBucket, 0, (len(level)+pyramidFanout-1)/pyramidFanout)
	for i := 0; i < len(level); i += pyramidFanout {
		out = append(out, mergeGroup(level[i:min(i+pyramidFanout, len(level))]))
	}
	return out
}

// ── Query ────────────────────────────────────────────────────────────────

// keyMeta is one key's parsed header entry.
type keyMeta struct {
	pointCount  int64
	layerStarts []int64 // coarsest first
	layerCounts []int64 // derived: record count per layer
}

// PyramidInfo is one key's header entry — the human-readable half of the
// format, for `runq metrics-index inspect` and tests.
type PyramidInfo struct {
	Key         string  `json:"key"`
	PointCount  int64   `json:"point_count"`
	LayerCounts []int64 `json:"layer_counts"` // records per layer, coarsest first
}

// InspectPyramid parses only the header and reports every key's shape.
func InspectPyramid(ctx context.Context, fsys rfs.FS, taskDir string) ([]PyramidInfo, error) {
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	f, err := fsys.Open(PyramidPath(taskDir))
	if err != nil {
		return nil, ErrPyramidNotBuilt
	}
	defer f.Close()
	metas, err := parseHeader(f)
	if err != nil {
		return nil, err
	}
	out := make([]PyramidInfo, 0, len(metas))
	for key, m := range metas {
		out = append(out, PyramidInfo{Key: key, PointCount: m.pointCount, LayerCounts: m.layerCounts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// QueryPyramid returns ≤ maxBuckets buckets for one key covering [fromTS, toTS]
// (0,0 = full range). Descent: start at the coarsest level, narrow the
// bucket span by first_ts/last_ts, drop one level (span ×pyramidFanout) while the
// next level's span still fits maxBuckets — each level is ONE contiguous
// ranged read, so a deep zoom costs O(log n) round trips with tiny reads,
// and the common full-range chart costs exactly one.
func QueryPyramid(ctx context.Context, fsys rfs.FS, taskDir, key string, fromTS, toTS int64, maxBuckets int) ([]PyramidBucket, error) {
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	if maxBuckets <= 0 {
		maxBuckets = 2000
	}
	f, err := fsys.Open(PyramidPath(taskDir))
	if err != nil {
		return nil, ErrPyramidNotBuilt
	}
	defer f.Close()

	metas, err := parseHeader(f)
	if err != nil {
		return nil, ErrPyramidNotBuilt // torn/invalid file = not built
	}
	meta, ok := metas[key]
	if !ok {
		return nil, fmt.Errorf("pyramid: key %q not indexed", key)
	}

	// Choose the coarsest level as the descent start; if even it exceeds
	// the overshoot budget (absurd maxBuckets), clamp by reading its head
	// slice.
	level := 0
	lo, hi := int64(0), meta.layerCounts[0] // [lo, hi) bucket span

	// Overshoot-then-merge: descending only while the child span fits the
	// budget quantizes display granularity in ×pyramidFanout jumps (a 2000
	// budget could return 501 buckets). Instead we allow ONE level past the
	// budget (read ≤ Fanout×maxBuckets records — still a single contiguous
	// ranged read, ~512KB at the default budget) and merge the result down
	// to exactly maxBuckets. Merging uses the same associative ops as the
	// builder, so the answer is exact, and granularity becomes continuous.
	overshoot := int64(maxBuckets) * pyramidFanout

	for {
		if hi-lo > overshoot {
			hi = lo + overshoot // degenerate clamp; callers pass sane budgets
		}
		buckets, err := readSlice(f, meta.layerStarts[level]+lo*pyramidRecordSize, int(hi-lo))
		if err != nil {
			return nil, err
		}
		// Narrow to the ts range within this level.
		if fromTS != 0 || toTS != 0 {
			nlo, nhi := narrowByTS(buckets, fromTS, toTS)
			lo, hi = lo+nlo, lo+nhi
			buckets = buckets[nlo:nhi]
		}
		// Descend while the finer level's span still fits the OVERSHOOT
		// budget (the final merge brings it back to maxBuckets exactly).
		if level+1 < len(meta.layerStarts) {
			clo, chi := lo*pyramidFanout, hi*pyramidFanout
			if chi > meta.layerCounts[level+1] {
				chi = meta.layerCounts[level+1]
			}
			if chi-clo <= overshoot {
				level++
				lo, hi = clo, chi
				continue
			}
		}
		return mergeToBudget(buckets, maxBuckets), nil
	}
}

// MergeBucketsToBudget is the exported re-bucketing entry: the tail-window
// fallback path builds 1-point buckets from raw points and merges them
// down with the SAME operator the pyramid uses — one aggregation semantic
// everywhere ("pyramid" and "tail" sources are indistinguishable except
// in precision of raw ranges).
func MergeBucketsToBudget(buckets []PyramidBucket, budget int) []PyramidBucket {
	return mergeToBudget(buckets, budget)
}

// mergeToBudget re-buckets n buckets into exactly ≤ budget buckets with
// near-equal group sizes (first `remainder` groups take one extra). Same
// associative ops as the builder's fold — lossless aggregation.
func mergeToBudget(buckets []PyramidBucket, budget int) []PyramidBucket {
	n := len(buckets)
	if n <= budget {
		return buckets
	}
	base, rem := n/budget, n%budget
	out := make([]PyramidBucket, 0, budget)
	i := 0
	for g := 0; g < budget; g++ {
		size := base
		if g < rem {
			size++
		}
		out = append(out, mergeGroup(buckets[i:i+size]))
		i += size
	}
	return out
}

// mergeGroup folds a run of buckets into one — THE associative operator,
// shared by the builder's foldLevel and the query's mergeToBudget so the
// two can never disagree.
func mergeGroup(g []PyramidBucket) PyramidBucket {
	m := g[0]
	for _, b := range g[1:] {
		m.Min = math.Min(m.Min, b.Min)
		m.Max = math.Max(m.Max, b.Max)
		m.Sum += b.Sum
		m.SumSq += b.SumSq
		m.Count += b.Count
		m.NaNCount += b.NaNCount
		if b.FirstTS < m.FirstTS {
			m.FirstTS = b.FirstTS
		}
		if b.LastTS > m.LastTS {
			m.LastTS = b.LastTS
		}
		if b.FirstStep != -1 && (m.FirstStep == -1 || b.FirstStep < m.FirstStep) {
			m.FirstStep = b.FirstStep
		}
		if b.LastStep > m.LastStep {
			m.LastStep = b.LastStep
		}
		if b.RawStart < m.RawStart {
			m.RawStart = b.RawStart
		}
		if b.RawEnd > m.RawEnd {
			m.RawEnd = b.RawEnd
		}
	}
	return m
}

// narrowByTS returns the [lo, hi) sub-span of buckets overlapping the ts
// range. Buckets are point-index ordered and ts ranges may overlap
// slightly (non-monotonic writers), so scan — spans here are ≤ maxBuckets.
func narrowByTS(buckets []PyramidBucket, fromTS, toTS int64) (int64, int64) {
	lo, hi := int64(0), int64(len(buckets))
	for lo < hi && fromTS != 0 && buckets[lo].LastTS < fromTS {
		lo++
	}
	for hi > lo && toTS != 0 && buckets[hi-1].FirstTS > toTS {
		hi--
	}
	return lo, hi
}

func readSlice(f rfs.File, off int64, n int) ([]PyramidBucket, error) {
	if n <= 0 {
		return nil, nil
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	raw := make([]byte, n*pyramidRecordSize)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, ErrPyramidNotBuilt // truncated data section = torn file
	}
	out := make([]PyramidBucket, n)
	for i := range out {
		out[i] = decodeBucket(raw[i*pyramidRecordSize:])
	}
	return out, nil
}

func parseHeader(f rfs.File) (map[string]keyMeta, error) {
	br := bufio.NewReaderSize(f, 64*1024)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != pyramidMagic {
		return nil, ErrPyramidNotBuilt
	}
	var ver uint8
	if err := binary.Read(br, binary.LittleEndian, &ver); err != nil || ver != pyramidVersion {
		return nil, ErrPyramidNotBuilt
	}
	var keyCount uint16
	if err := binary.Read(br, binary.LittleEndian, &keyCount); err != nil {
		return nil, err
	}
	metas := make(map[string]keyMeta, keyCount)
	for i := 0; i < int(keyCount); i++ {
		var klen uint16
		if err := binary.Read(br, binary.LittleEndian, &klen); err != nil {
			return nil, err
		}
		name := make([]byte, klen)
		if _, err := io.ReadFull(br, name); err != nil {
			return nil, err
		}
		var pc uint64
		if err := binary.Read(br, binary.LittleEndian, &pc); err != nil {
			return nil, err
		}
		var layerCount uint8
		if err := binary.Read(br, binary.LittleEndian, &layerCount); err != nil {
			return nil, err
		}
		starts := make([]int64, layerCount)
		for j := range starts {
			var s uint64
			if err := binary.Read(br, binary.LittleEndian, &s); err != nil {
				return nil, err
			}
			starts[j] = int64(s)
		}
		// Derive per-layer record counts from the leaf count upward, then
		// reverse to coarsest-first (same arithmetic the builder used —
		// no counts stored, none can disagree).
		leaves := (int64(pc) + pyramidLeafWidth - 1) / pyramidLeafWidth
		counts := []int64{leaves}
		for counts[len(counts)-1] > 1 {
			counts = append(counts, (counts[len(counts)-1]+pyramidFanout-1)/pyramidFanout)
		}
		for l, r := 0, len(counts)-1; l < r; l, r = l+1, r-1 {
			counts[l], counts[r] = counts[r], counts[l]
		}
		if len(counts) != int(layerCount) {
			return nil, ErrPyramidNotBuilt // header inconsistent with arithmetic
		}
		metas[string(name)] = keyMeta{pointCount: int64(pc), layerStarts: starts, layerCounts: counts}
	}
	return metas, nil
}

// ── binary helpers ──────────────────────────────────────────────────────

type leWriter struct{ buf []byte }

func newLEWriter(buf []byte) *leWriter { return &leWriter{buf: buf} }

func (w *leWriter) bytes(b []byte) { w.buf = append(w.buf, b...) }
func (w *leWriter) u8(v uint8)     { w.buf = append(w.buf, v) }
func (w *leWriter) u16(v uint16)   { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *leWriter) u64(v uint64)   { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }
func (w *leWriter) f64(v float64)  { w.u64(math.Float64bits(v)) }

func (w *leWriter) bucket(b PyramidBucket) {
	w.f64(b.Min)
	w.f64(b.Max)
	w.f64(b.Sum)
	w.f64(b.SumSq)
	w.u64(uint64(b.Count))
	w.u64(uint64(b.NaNCount))
	w.u64(uint64(b.FirstTS))
	w.u64(uint64(b.LastTS))
	w.u64(uint64(b.FirstStep))
	w.u64(uint64(b.LastStep))
	w.u64(uint64(b.RawStart))
	w.u64(uint64(b.RawEnd))
}

func decodeBucket(raw []byte) PyramidBucket {
	u := func(i int) uint64 { return binary.LittleEndian.Uint64(raw[i*8:]) }
	return PyramidBucket{
		Min:       math.Float64frombits(u(0)),
		Max:       math.Float64frombits(u(1)),
		Sum:       math.Float64frombits(u(2)),
		SumSq:     math.Float64frombits(u(3)),
		Count:     int64(u(4)),
		NaNCount:  int64(u(5)),
		FirstTS:   int64(u(6)),
		LastTS:    int64(u(7)),
		FirstStep: int64(u(8)),
		LastStep:  int64(u(9)),
		RawStart:  int64(u(10)),
		RawEnd:    int64(u(11)),
	}
}
