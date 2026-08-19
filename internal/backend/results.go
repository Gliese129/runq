package backend

// Columnar assembly for GET /jobs/{id}/results (RQ2-1 §A) — pure SQL over
// result_records, zero filesystem access. Shared by LocalBackend and
// SSHBackend (which runs EnsureFresh first so the store is current).
//
// Key classification ("smart parse") doctrine — ZERO path/name inference:
//   - `model` is the ONLY convention identity key (role=identity), typed —
//     model=1 and model="1" are distinct series (see identityKey). Records
//     without it fall back to their task_id — a fact of the write contract,
//     not a guess (each task then forms its own series: ablation semantics).
//   - Any other axis whose values are numeric AND vary within at least one
//     series is an x candidate (role=x), first-appearance order, first =
//     primary. Everything else is a label.
//   - Mixed-type axes: majority type wins (tie prefers num), minority
//     values are nulled and tallied in schema.axes[name].nulled — the
//     warning travels with the data.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/gliese129/runq/internal/store"
)

// ResultSource is the JobResults.Source constant: it names the write
// contract that feeds this endpoint, not a location.
const ResultSource = "runq.record(**axes)"

// resultRec is one parsed result_records row during assembly.
type resultRec struct {
	taskID   string
	ts       int64
	axes     map[string]any // float64 | string | bool | (non-scalar → nulled)
	metrics  map[string]any
	identity string // internal TYPED grouping key (see identityKey)
	label    string // display projection → schema.groups[].key
	order    int    // ingest order within the query (ties stay stable)
}

// axisType classifies one JSON value: "num" / "str" / "bool", "" = non-scalar.
func axisType(v any) string {
	switch v.(type) {
	case float64:
		return "num"
	case string:
		return "str"
	case bool:
		return "bool"
	default:
		return ""
	}
}

// identityKey builds a record's grouping identity. The INTERNAL key is
// type-tagged ("s:a" / "n:1" / "b:true", "task:<id>" for the fallback) so
// legal-but-distinct values like model=1 vs model="1" never merge, and a
// task-fallback can never collide with an explicit model value. The
// DISPLAY label is the untyped rendering — two groups may then show the
// same label, which is honest: they really are different series,
// distinguishable by their ranges and axis columns.
func identityKey(v any, taskID string) (internal, label string) {
	switch t := v.(type) {
	case string:
		return "s:" + t, t
	case float64:
		s := strconv.FormatFloat(t, 'g', -1, 64)
		return "n:" + s, s
	case bool:
		s := strconv.FormatBool(t)
		return "b:" + s, s
	default: // absent or non-scalar → per-task series (ablation semantics)
		return "task:" + taskID, taskID
	}
}

func jobResultsFromDB(ctx context.Context, st *store.Store, jobID string) (*JobResults, error) {
	rows, err := st.ListResultRecords(ctx, jobID)
	if err != nil {
		return nil, err
	}
	dropped, err := st.SumResultsDropped(ctx, jobID)
	if err != nil {
		return nil, err
	}

	recs := make([]*resultRec, 0, len(rows))
	for i, row := range rows {
		var axes map[string]any
		var metrics map[string]any
		// Ingest validated object-ness; a parse failure here means store
		// corruption — skip the row rather than 500 the whole response.
		if err := json.Unmarshal([]byte(row.AxesJSON), &axes); err != nil {
			logger.Warnf("results %s: task %s: corrupt axes_json (row skipped): %v", jobID, row.TaskID, err)
			continue
		}
		if err := json.Unmarshal([]byte(row.MetricsJSON), &metrics); err != nil {
			logger.Warnf("results %s: task %s: corrupt metrics_json (row skipped): %v", jobID, row.TaskID, err)
			continue
		}
		rec := &resultRec{taskID: row.TaskID, ts: row.TS, axes: axes, metrics: metrics, order: i}
		rec.identity, rec.label = identityKey(axes["model"], row.TaskID)
		recs = append(recs, rec)
	}

	out := &JobResults{
		Source:    ResultSource,
		Parsed:    len(recs),
		Skipped:   dropped,
		Truncated: dropped > 0,
		N:         len(recs),
		Schema: ResultSchema{
			Groups:  []ResultRange{},
			Tasks:   []ResultRange{},
			Axes:    map[string]ResultAxis{},
			XAxes:   []string{},
			Metrics: []string{},
		},
		Cols: ResultCols{
			TS:      []int64{},
			Axes:    map[string][]any{},
			Metrics: map[string][]*float64{},
		},
	}
	if len(recs) == 0 {
		return out, nil
	}

	// ---- axis discovery + majority type vote ----
	type axisVote struct {
		firstSeen int
		counts    map[string]int
	}
	votes := map[string]*axisVote{}
	axisOrder := []string{}
	for i, rec := range recs {
		for k, v := range rec.axes {
			av, ok := votes[k]
			if !ok {
				av = &axisVote{firstSeen: i, counts: map[string]int{}}
				votes[k] = av
				axisOrder = append(axisOrder, k)
			}
			av.counts[axisType(v)]++
		}
	}
	// First-appearance order at RECORD granularity; axes debuting in the
	// same record tie-break by name (map iteration order must not leak
	// into x_axes / primary-x selection).
	sort.SliceStable(axisOrder, func(i, j int) bool {
		a, b := votes[axisOrder[i]], votes[axisOrder[j]]
		if a.firstSeen != b.firstSeen {
			return a.firstSeen < b.firstSeen
		}
		return axisOrder[i] < axisOrder[j]
	})
	winner := map[string]string{}
	for k, av := range votes {
		best, bestN := "num", -1
		for _, t := range []string{"num", "str", "bool"} { // tie prefers num
			if av.counts[t] > bestN {
				best, bestN = t, av.counts[t]
			}
		}
		winner[k] = best
	}

	// ---- metric key discovery (sorted: stable display order) ----
	metricSeen := map[string]bool{}
	metricKeys := []string{}
	for _, rec := range recs {
		for k := range rec.metrics {
			if !metricSeen[k] {
				metricSeen[k] = true
				metricKeys = append(metricKeys, k)
			}
		}
	}
	sort.Strings(metricKeys)

	// ---- x candidacy: numeric axis varying within at least one series ----
	numVal := func(rec *resultRec, k string) (float64, bool) {
		if winner[k] != "num" {
			return 0, false
		}
		f, ok := rec.axes[k].(float64)
		return f, ok
	}
	xAxes := []string{}
	for _, k := range axisOrder {
		if k == "model" || winner[k] != "num" {
			continue
		}
		distinct := map[string]map[float64]bool{}
		varying := false
		for _, rec := range recs {
			v, ok := numVal(rec, k)
			if !ok {
				continue
			}
			set := distinct[rec.identity]
			if set == nil {
				set = map[float64]bool{}
				distinct[rec.identity] = set
			}
			set[v] = true
			if len(set) > 1 {
				varying = true
				break
			}
		}
		if varying {
			xAxes = append(xAxes, k)
		}
	}
	primaryX := ""
	if len(xAxes) > 0 {
		primaryX = xAxes[0]
	}

	// ---- sort: (identity, primary x [nulls last], ts, ingest order) ----
	// task is deliberately NOT a sort key: within a series the sequence IS
	// the curve, and every table slice (last / first / aligned-at-x*) is a
	// group-range operation that needs x monotonic across tasks — a series
	// resumed in a second task must interleave by x, not cluster by task
	// (Codex r1 finding 1). Task runs split accordingly; schema.tasks
	// carries one entry per contiguous run.
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.identity != b.identity {
			return a.identity < b.identity
		}
		if primaryX != "" {
			av, aok := numVal(a, primaryX)
			bv, bok := numVal(b, primaryX)
			if aok != bok {
				return aok // non-null before null
			}
			if aok && av != bv {
				return av < bv
			}
		}
		if a.ts != b.ts {
			return a.ts < b.ts
		}
		return a.order < b.order
	})

	// ---- ranges: identity runs and task runs (scan of the sorted seq) ----
	for i := range recs {
		newGroup := i == 0 || recs[i].identity != recs[i-1].identity
		if newGroup {
			out.Schema.Groups = append(out.Schema.Groups, ResultRange{Key: recs[i].label, Offset: i})
		}
		out.Schema.Groups[len(out.Schema.Groups)-1].Count++
		// Task runs NEST inside groups: a run breaks on a group boundary
		// even when the task id continues (Codex r1 finding 2) — a
		// per-(group, task) slice must never leak another model's rows.
		if newGroup || recs[i].taskID != recs[i-1].taskID {
			out.Schema.Tasks = append(out.Schema.Tasks, ResultRange{ID: recs[i].taskID, Offset: i})
		}
		out.Schema.Tasks[len(out.Schema.Tasks)-1].Count++
	}

	// ---- columns ----
	n := len(recs)
	out.Cols.TS = make([]int64, n)
	updated := int64(0)
	for i, rec := range recs {
		out.Cols.TS[i] = rec.ts
		if rec.ts > updated {
			updated = rec.ts
		}
	}
	out.UpdatedAt = updated

	for _, k := range axisOrder {
		w := winner[k]
		role := "label"
		if k == "model" {
			role = "identity"
		} else {
			for _, x := range xAxes {
				if x == k {
					role = "x"
					break
				}
			}
		}
		ax := ResultAxis{Type: w, Role: role}
		col := make([]any, n)
		vocabIdx := map[string]int{}
		for i, rec := range recs {
			v, present := rec.axes[k]
			if !present {
				continue // absent → null (participation hole, not a conflict)
			}
			if axisType(v) != w {
				ax.Nulled++ // minority type or non-scalar → null, tallied
				continue
			}
			switch w {
			case "num":
				col[i] = v.(float64)
			case "bool":
				col[i] = v.(bool)
			case "str":
				s := v.(string)
				idx, ok := vocabIdx[s]
				if !ok {
					idx = len(ax.Vocab)
					vocabIdx[s] = idx
					ax.Vocab = append(ax.Vocab, s)
				}
				col[i] = idx
			}
		}
		if ax.Nulled > 0 {
			logger.Warnf("results %s: axis %q has mixed/non-scalar values (%d nulled, kept type %s)", jobID, k, ax.Nulled, w)
		}
		out.Schema.Axes[k] = ax
		out.Cols.Axes[k] = col
	}

	for _, k := range metricKeys {
		col := make([]*float64, n)
		for i, rec := range recs {
			if f, ok := rec.metrics[k].(float64); ok {
				v := f
				col[i] = &v
			}
			// absent or non-numeric (hand-written data) → null hole
		}
		out.Cols.Metrics[k] = col
	}

	out.Schema.XAxes = xAxes
	out.Schema.Metrics = metricKeys
	return out, nil
}
