package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"

	"github.com/bytedance/gopkg/util/logger"
	"github.com/gliese129/runq-lab/internal/rfs"
	"github.com/gliese129/runq-lab/internal/store"
	"github.com/gliese129/runq-lab/internal/workspace"
)

// Target identifies the task workspace whose SDK outputs should be ingested.
type Target struct {
	TaskID string
	JobID  string
	Dir    string

	// FS is the filesystem Dir lives on. nil = local (os semantics). Remote
	// backends pass their rfs.SSHFS so metrics.jsonl on the cluster is
	// actually readable — a bare os.Open of a remote path silently reads
	// nothing.
	FS rfs.FS
}

// Result summarizes what was ingested from the task's SDK output files.
// Counts are informational; no decision logic depends on them.
type Result struct {
	MetricCount     int
	CheckpointCount int
	ResultCount     int // result records parsed from results.jsonl (pre-cap)
}

// SDK three-file contract (RQ2-1) — each stream has its own file so the
// semantics split at the API boundary and ingest never disambiguates:
//
//	metrics.jsonl — {"type":"metric","key":"loss","value":0.42,"step":100,"ts":...}
//	events.jsonl  — {"type":"checkpoint","path":"...","size_bytes":1024,"step":100,"is_best":true,"ts":...}
//	                (+ "preempted" / "loop_break": flight-recorder only, no DB rows)
//	results.jsonl — {"ts":...,"axes":{"model":"a","step":2000},"metrics":{"math":24.8}}
//
// OLD tasks' mixed metrics.jsonl (pre-split SDKs wrote checkpoint events
// there too) stays fully supported: the metrics collector still dispatches
// checkpoint lines. Zero migration.
//
// `task_id` / `job_id` are NOT in the SDK payload. The caller supplies them
// through Target, keeping the SDK ignorant of internal DB schemas.
type metricEvent struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	Step  *int64  `json:"step"`
	TS    int64   `json:"ts"`
}

type checkpointEvent struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Step      *int64 `json:"step"`
	IsBest    bool   `json:"is_best"`
	TS        int64  `json:"ts"`
}

// NOTE: the legacy ReapOutputs (markless full-file reap) is GONE. Its
// "idempotent via INSERT OR IGNORE" contract silently broke when the
// point table died: streaming summaries ACCUMULATE, so a markless re-read
// double-counts. ReapIncremental's (size, parsed_offset) mark is what
// makes re-reading safe now — every caller must go through it.
//
// SDK event vocabulary and error posture (unchanged):
//   - unknown event types are debug-logged and skipped (forward
//     compatibility), bad JSON lines warn + skip.
//   - Reap MUST NOT propagate errors that would change a task's terminal
//     status: verdict-path callers treat returned errors as warn-only.
//   - disk_low events fall through to "unknown type": the SDK-driven
//     freeze model handles them at runtime via /internal/freeze-self.

// collector performs the STREAMING REDUCTION over metrics.jsonl lines.
// Raw metric points are never stored (a chatty 3-day task is 10M+ points):
// each point folds into its (key) summary on the fly; charts read the raw
// tail window or the on-target pyramid index instead.
type collector struct {
	target Target
	path   string
	lineN  int
	aggs   map[string]*store.MetricSummaryRow
	ckpts  []store.CheckpointRow
	result Result
}

func (c *collector) line(lineBytes []byte) {
	c.lineN++
	if len(lineBytes) == 0 {
		return
	}
	var t struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(lineBytes, &t); err != nil {
		logger.Warnf("reap %s line %d: parse type discriminator failed: %v", c.path, c.lineN, err)
		return
	}
	switch t.Type {
	case "metric":
		var e metricEvent
		if err := json.Unmarshal(lineBytes, &e); err != nil {
			logger.Warnf("reap %s line %d: parse metric event failed: %v", c.path, c.lineN, err)
			return
		}
		if c.aggs == nil {
			c.aggs = make(map[string]*store.MetricSummaryRow)
		}
		agg, ok := c.aggs[e.Key]
		if !ok {
			c.aggs[e.Key] = &store.MetricSummaryRow{
				TaskID: c.target.TaskID, JobID: c.target.JobID, Key: e.Key,
				Min: e.Value, Max: e.Value, Last: e.Value, LastTS: e.TS, Count: 1,
			}
		} else {
			agg.Min = min(agg.Min, e.Value)
			agg.Max = max(agg.Max, e.Value)
			if e.TS >= agg.LastTS {
				agg.Last, agg.LastTS = e.Value, e.TS
			}
			agg.Count++
		}
		c.result.MetricCount++
	case "checkpoint":
		var e checkpointEvent
		if err := json.Unmarshal(lineBytes, &e); err != nil {
			logger.Warnf("reap %s line %d: parse checkpoint event failed: %v", c.path, c.lineN, err)
			return
		}
		c.ckpts = append(c.ckpts, store.CheckpointRow{
			TaskID:    c.target.TaskID,
			JobID:     c.target.JobID,
			Path:      e.Path,
			SizeBytes: e.SizeBytes,
			Step:      e.Step,
			IsBest:    e.IsBest,
			TS:        e.TS,
		})
		c.result.CheckpointCount++
	default:
		logger.Debugf("reap %s line %d: unknown event type %q (skipped)", c.path, c.lineN, t.Type)
	}
}

// summaryRows materializes the per-key reductions for the atomic apply.
func (c *collector) summaryRows() []store.MetricSummaryRow {
	rows := make([]store.MetricSummaryRow, 0, len(c.aggs))
	for _, a := range c.aggs {
		rows = append(rows, *a)
	}
	return rows
}

// eventsCollector handles events.jsonl (lifecycle plane): checkpoint events
// become rows; preempted / loop_break are flight-recorder entries with no
// DB consumers (post-mortem evidence read from the file itself) — skipped.
// A stray metric event here is a contract violation, NOT summarized: doing
// so could double-count against metrics.jsonl.
type eventsCollector struct {
	target Target
	path   string
	lineN  int
	ckpts  []store.CheckpointRow
	count  int
}

func (c *eventsCollector) line(lineBytes []byte) {
	c.lineN++
	if len(lineBytes) == 0 {
		return
	}
	var t struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(lineBytes, &t); err != nil {
		logger.Warnf("reap %s line %d: parse type discriminator failed: %v", c.path, c.lineN, err)
		return
	}
	switch t.Type {
	case "checkpoint":
		var e checkpointEvent
		if err := json.Unmarshal(lineBytes, &e); err != nil {
			logger.Warnf("reap %s line %d: parse checkpoint event failed: %v", c.path, c.lineN, err)
			return
		}
		c.ckpts = append(c.ckpts, store.CheckpointRow{
			TaskID:    c.target.TaskID,
			JobID:     c.target.JobID,
			Path:      e.Path,
			SizeBytes: e.SizeBytes,
			Step:      e.Step,
			IsBest:    e.IsBest,
			TS:        e.TS,
		})
		c.count++
	default:
		logger.Debugf("reap %s line %d: event type %q not ingested (skipped)", c.path, c.lineN, t.Type)
	}
}

// resultLine maps one results.jsonl line. Axes/metrics stay raw JSON —
// key classification is the results endpoint's concern.
type resultLine struct {
	TS      int64           `json:"ts"`
	Axes    json.RawMessage `json:"axes"`
	Metrics json.RawMessage `json:"metrics"`
}

type resultCollector struct {
	target Target
	path   string
	lineN  int
	parsed int // valid records seen (pre-cap)
	rows   []store.ResultRecordRow
}

func (c *resultCollector) line(lineBytes []byte) {
	c.lineN++
	if len(lineBytes) == 0 {
		return
	}
	var rl resultLine
	if err := json.Unmarshal(lineBytes, &rl); err != nil {
		logger.Warnf("reap %s line %d: parse result record failed: %v", c.path, c.lineN, err)
		return
	}
	axes, ok := normalizeJSONObject(rl.Axes)
	if !ok {
		logger.Warnf("reap %s line %d: axes is not a JSON object (skipped)", c.path, c.lineN)
		return
	}
	metrics, ok := normalizeJSONObject(rl.Metrics)
	if !ok {
		logger.Warnf("reap %s line %d: metrics is not a JSON object (skipped)", c.path, c.lineN)
		return
	}
	c.parsed++
	// Materialize at most the cap — the DB can never have room for more;
	// the parsed tally beyond it feeds dropped_count in the apply.
	if len(c.rows) < store.MaxResultRecordsPerTask {
		c.rows = append(c.rows, store.ResultRecordRow{
			TaskID: c.target.TaskID, JobID: c.target.JobID,
			TS: rl.TS, AxesJSON: axes, MetricsJSON: metrics,
		})
	}
}

// normalizeJSONObject validates raw as a JSON object and compacts it.
// Absent / null → "{}" (the SDK always writes both fields; tolerate
// hand-written lines that omit one).
func normalizeJSONObject(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "{}", true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", false
	}
	return buf.String(), true
}

// scanOutcome reports one incremental pass over a single jsonl file.
type scanOutcome struct {
	missing  bool  // file does not exist
	skip     bool  // no new bytes (and not a final pass)
	rebuild  bool  // file shrank — caller must rebuild derived rows
	size     int64 // stat size at this pass
	consumed int64 // parsed up to here (complete lines; tail included when final)
}

// scanIncremental applies the (size, parsed_offset) incremental-read
// doctrine (spec §8.1.4) to ONE file, feeding each complete line to onLine:
//
//	size unchanged → zero transfer, zero parse (the common idle case)
//	size grew      → seek to parsed_offset, parse the delta's COMPLETE
//	                 lines (an unterminated tail waits for the next pass)
//	size shrank    → the file was rewritten (retry rerun): rebuild from 0
//
// final consumes the tail even without a trailing newline. The function
// performs NO store writes — pairing the parsed delta with its mark update
// atomically is the caller's job.
func scanIncremental(fsys rfs.FS, path string, markSize, markOffset int64, final bool, onLine func([]byte)) (scanOutcome, error) {
	info, err := fsys.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return scanOutcome{missing: true}, nil
	}
	if err != nil {
		return scanOutcome{}, err
	}

	size := info.Size()
	offset := markOffset
	rebuild := false
	if size < markSize {
		rebuild = true
		offset = 0
	} else if size == markSize && !final {
		return scanOutcome{skip: true, size: size}, nil
	}

	f, err := fsys.Open(path)
	if err != nil {
		return scanOutcome{}, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return scanOutcome{}, err
		}
	}

	br := bufio.NewReaderSize(f, 64*1024)
	consumed := offset
	for {
		raw, rerr := br.ReadBytes('\n')
		complete := rerr == nil
		if len(raw) > 0 && (complete || final) {
			onLine(bytes.TrimRight(raw, "\r\n"))
			consumed += int64(len(raw))
		}
		if rerr != nil {
			if rerr != io.EOF {
				logger.Warnf("reap %s: read error (partial result): %v", path, rerr)
			}
			break
		}
	}
	return scanOutcome{rebuild: rebuild, size: size, consumed: consumed}, nil
}

// ReapIncremental ingests the bytes appended to the task's SDK output files
// (metrics.jsonl / results.jsonl / events.jsonl) since the last pass, each
// under its own (size, parsed_offset) mark. final marks the terminal pass:
// tails are consumed and every mark is frozen — settled tasks are never
// stat'ed again. Per-file failures don't block the other files; errors are
// joined, and remain warn-only for callers on verdict paths (same contract
// as always).
func ReapIncremental(ctx context.Context, st *store.Store, target Target, final bool) (Result, error) {
	fsys := target.FS
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	var res Result
	merr := reapMetricsFile(ctx, st, target, fsys, final, &res)
	rerr := reapResultsFile(ctx, st, target, fsys, final, &res)
	eerr := reapEventsFile(ctx, st, target, fsys, final, &res)
	return res, errors.Join(merr, rerr, eerr)
}

func reapMetricsFile(ctx context.Context, st *store.Store, target Target, fsys rfs.FS, final bool, res *Result) error {
	mark, err := st.GetIngestMark(ctx, target.TaskID)
	if err != nil {
		return err
	}
	if mark.Final {
		return nil
	}
	path := workspace.MetricsPath(target.Dir)
	c := collector{target: target, path: path}
	sc, err := scanIncremental(fsys, path, mark.Size, mark.Offset, final, c.line)
	if err != nil {
		return err
	}
	if sc.missing {
		if final { // no-SDK task: freeze so we never look again
			_ = st.SetIngestMark(ctx, target.TaskID, store.IngestMark{Final: true})
		}
		return nil
	}
	if sc.skip {
		return nil
	}
	// ONE transaction: rebuild delete + summary merges + checkpoints + the
	// mark. "Counted" and "accounted" are inseparable — a crash between
	// them would replay this delta next pass and inflate count/sum.
	if err := st.ApplyIngestDelta(ctx, target.TaskID, sc.rebuild,
		c.summaryRows(), c.ckpts,
		store.IngestMark{Size: sc.size, Offset: sc.consumed, Final: final}); err != nil {
		return err
	}
	res.MetricCount += c.result.MetricCount
	res.CheckpointCount += c.result.CheckpointCount
	return nil
}

func reapResultsFile(ctx context.Context, st *store.Store, target Target, fsys rfs.FS, final bool, res *Result) error {
	mark, err := st.GetFileIngestMark(ctx, target.TaskID, store.IngestFileResults)
	if err != nil {
		return err
	}
	if mark.Final {
		return nil
	}
	path := workspace.ResultsPath(target.Dir)
	c := resultCollector{target: target, path: path}
	sc, err := scanIncremental(fsys, path, mark.Size, mark.Offset, final, c.line)
	if err != nil {
		return err
	}
	if sc.missing {
		if final {
			_ = st.SetFileIngestMark(ctx, target.TaskID, store.IngestFileResults, store.FileIngestMark{Final: true})
		}
		return nil
	}
	if sc.skip {
		return nil
	}
	if err := st.ApplyResultsIngestDelta(ctx, target.TaskID, sc.rebuild,
		c.rows, c.parsed,
		store.FileIngestMark{Size: sc.size, Offset: sc.consumed, Final: final}); err != nil {
		return err
	}
	res.ResultCount += c.parsed
	return nil
}

func reapEventsFile(ctx context.Context, st *store.Store, target Target, fsys rfs.FS, final bool, res *Result) error {
	mark, err := st.GetFileIngestMark(ctx, target.TaskID, store.IngestFileEvents)
	if err != nil {
		return err
	}
	if mark.Final {
		return nil
	}
	path := workspace.EventsPath(target.Dir)
	c := eventsCollector{target: target, path: path}
	sc, err := scanIncremental(fsys, path, mark.Size, mark.Offset, final, c.line)
	if err != nil {
		return err
	}
	if sc.missing {
		if final {
			_ = st.SetFileIngestMark(ctx, target.TaskID, store.IngestFileEvents, store.FileIngestMark{Final: true})
		}
		return nil
	}
	if sc.skip {
		return nil
	}
	// Rebuild needs no delete: checkpoints re-insert idempotently via the
	// (task_id, step) newer-ts upsert (task-lifetime event log doctrine).
	if err := st.ApplyEventsIngestDelta(ctx, target.TaskID, c.ckpts,
		store.FileIngestMark{Size: sc.size, Offset: sc.consumed, Final: final}); err != nil {
		return err
	}
	res.CheckpointCount += c.count
	return nil
}
