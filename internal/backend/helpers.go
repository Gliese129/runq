package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/logfile"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/utils"
	"github.com/gliese129/runq/internal/workspace"
)

// ---- builders: store rows → view types ----

func BuildJobSummary(job store.JobRow, tasks []store.TaskRow) JobSummary {
	counts := TaskCountGroup{Total: len(tasks)}
	for _, task := range tasks {
		switch task.Status {
		case "pending":
			counts.Pending++
		case "running":
			counts.Running++
		case "unknown":
			counts.Unknown++
		case "success":
			counts.Completed++
		case "failed", "killed":
			counts.Failed++
		}
	}

	var refreshedAt *int64
	if job.RefreshedAt != nil {
		ts := job.RefreshedAt.Unix()
		refreshedAt = &ts
	}

	return JobSummary{
		ID:          job.ID,
		Project:     job.ProjectName,
		Note:        job.Note,
		Status:      job.Status,
		Target:      job.Target,
		Archived:    job.ArchivedAt != nil,
		CreatedAt:   job.CreatedAt.Unix(),
		Tasks:       counts,
		RefreshedAt: refreshedAt,
	}
}

// BuildJobDetail assembles the detail view. metricKeys comes from the
// caller's store (SELECT DISTINCT over summaries) — the old file-scanning
// key discovery is gone: it capped at 5 tasks and silently returned
// nothing for remote task dirs.
func BuildJobDetail(job store.JobRow, tasks []store.TaskRow, metricKeys []string) JobDetail {
	views := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, BuildTaskView(task))
	}
	return JobDetail{
		Job:        BuildJobSummary(job, tasks),
		Tasks:      views,
		MetricKeys: metricKeys,
		// Raw config (note template, sweep blocks) — powers "re-run as
		// template" in the GUI without a second endpoint.
		Config:  json.RawMessage(job.ConfigJSON),
		DataDir: jobDataDir(tasks),
	}
}

// jobDataDir resolves the job's workspace directory from recorded task
// dirs (task_dir = <jobRoot>/<taskID>, stamped at submit) — a fact from
// the ledger, not a re-derivation of root + note slug that could drift
// from what submit actually did. Empty when no task carries a dir.
func jobDataDir(tasks []store.TaskRow) string {
	for _, t := range tasks {
		if t.TaskDir != "" {
			// Both lanes join with forward slashes (workspace.TaskDir).
			return path.Dir(t.TaskDir)
		}
	}
	return ""
}

// applyTaskDetail adds the detail-only execution facts to a list-shaped
// TaskView (RQ2-1 §G) — shared by every GetTask implementation so the
// detail response is uniform across lanes.
func applyTaskDetail(view *TaskView, task store.TaskRow) {
	view.Command = task.Command
	view.WorkingDir = task.WorkingDir
	view.TaskDir = task.TaskDir
}

// compareRowsFromDB builds CompareRows from INGESTED metrics — pure SQL,
// no file IO (spec §8.1.4: the background ingest keeps the DB warm).
func compareRowsFromDB(ctx context.Context, st *store.Store, jobID, key string, desc bool) ([]CompareRow, error) {
	scores, err := st.MetricLeaderboard(ctx, jobID, key, desc)
	if err != nil {
		return nil, err
	}
	rows := make([]CompareRow, 0, len(scores))
	for i, sc := range scores {
		rows = append(rows, CompareRow{
			TaskID:   sc.TaskID,
			Status:   sc.Status,
			Params:   sc.Params,
			Best:     sc.Value,
			HasValue: sc.HasValue,
			Rank:     i + 1,
		})
	}
	return rows, nil
}

// tailMetricWindowBytes bounds the raw tail read for task charts: recent
// points come straight from metrics.jsonl's tail (raw points are never
// stored in the DB — see metric_summary / the pyramid index).
const tailMetricWindowBytes = 256 * 1024

// readTailMetricPoints reads the tail window of metrics.jsonl through the
// given FS and parses metric events — the chart path for running tasks
// (and the fallback for finished ones until their pyramid is built).
// Returns the newest ≤ maxPoints points; afterTS > 0 filters older ones
// (?after= incremental pull); key != "" keeps only that key.
func readTailMetricPoints(fsys rfs.FS, taskDir, key string, maxPoints int, afterTS int64) []MetricPoint {
	if taskDir == "" {
		return nil
	}
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	p := workspace.MetricsPath(taskDir)
	info, err := fsys.Stat(p)
	if err != nil {
		return nil
	}
	start := info.Size() - tailMetricWindowBytes
	if start < 0 {
		start = 0
	}
	f, err := fsys.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil
		}
	}

	br := bufio.NewReaderSize(f, 64*1024)
	if start > 0 {
		_, _ = br.ReadBytes('\n') // mid-line entry: discard the partial first line
	}
	var points []MetricPoint
	for {
		raw, rerr := br.ReadBytes('\n')
		if rerr == nil && len(raw) > 1 {
			var e struct {
				Type  string  `json:"type"`
				Key   string  `json:"key"`
				Value float64 `json:"value"`
				Step  *int    `json:"step"`
				TS    int64   `json:"ts"`
			}
			if json.Unmarshal(raw, &e) == nil && e.Type == "metric" &&
				(afterTS == 0 || e.TS > afterTS) && (key == "" || e.Key == key) {
				points = append(points, MetricPoint{Key: e.Key, Value: e.Value, Step: e.Step, TS: e.TS})
			}
		}
		if rerr != nil {
			break
		}
	}
	if len(points) > maxPoints {
		points = points[len(points)-maxPoints:]
	}
	return points
}

// tailMetricBuckets is the buckets-mode FALLBACK (running task / pyramid
// not built): tail points → 1-point buckets → MergeBucketsToBudget. Same
// aggregation operator as the pyramid, so the frontend renders both
// sources identically; RawStart/RawEnd are 0 (the tail path doesn't track
// raw offsets — zoom-to-raw needs the pyramid).
func tailMetricBuckets(fsys rfs.FS, taskDir, key string, fromTS, toTS int64, budget int) []workspace.PyramidBucket {
	points := readTailMetricPoints(fsys, taskDir, key, 1<<20, 0) // window-bounded anyway
	buckets := make([]workspace.PyramidBucket, 0, len(points))
	for _, p := range points {
		if fromTS != 0 && p.TS < fromTS {
			continue
		}
		if toTS != 0 && p.TS > toTS {
			continue
		}
		b := workspace.PyramidBucket{
			Min: p.Value, Max: p.Value, Sum: p.Value, SumSq: p.Value * p.Value,
			Count: 1, FirstTS: p.TS, LastTS: p.TS, FirstStep: -1, LastStep: -1,
		}
		if p.Step != nil {
			b.FirstStep, b.LastStep = int64(*p.Step), int64(*p.Step)
		}
		buckets = append(buckets, b)
	}
	return workspace.MergeBucketsToBudget(buckets, budget)
}

// jobLogSearchViaExec greps every task log of a job ON THE OWNING SIDE
// (RQ-44: results travel, files don't — one grep beats N file transfers).
// Fixed-string search (-F): the query is user text, not a regex — no
// injection semantics to reason about. Batched to keep command lines
// bounded on thousand-task sweeps.
func jobLogSearchViaExec(ctx context.Context, st *store.Store, fsys rfs.FS, jobID, query string) ([]LogMatch, error) {
	if query == "" {
		return []LogMatch{}, nil
	}
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	rows, err := st.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}

	const (
		maxMatches = 500 // mirrors logfile.MaxSearchMatches
		batchSize  = 100 // paths per grep invocation (command-line budget)
	)
	byPath := make(map[string]string, len(rows)) // log path → task id
	paths := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.LogPath != "" {
			byPath[r.LogPath] = r.ID
			paths = append(paths, r.LogPath)
		}
	}

	out := []LogMatch{}
	for start := 0; start < len(paths) && len(out) < maxMatches; start += batchSize {
		batch := paths[start:min(start+batchSize, len(paths))]
		var sb strings.Builder
		// -H force path prefix (even single file), -n line numbers,
		// -F fixed string, -s suppress missing-file noise (pending tasks),
		// -m per-file cap so one chatty log can't starve the others.
		sb.WriteString("grep -H -n -s -F -m 50 -- ")
		sb.WriteString(utils.ShellQuote(query))
		for _, p := range batch {
			sb.WriteByte(' ')
			sb.WriteString(utils.ShellQuote(p))
		}
		stdout, _, code, err := fsys.Exec(ctx, "sh", "-c", sb.String())
		if err != nil {
			return nil, fmt.Errorf("job log search: %w", err) // transport: no fact learned
		}
		if code > 1 && len(stdout) == 0 {
			return nil, fmt.Errorf("job log search: grep exit %d", code)
		}
		// exit 1 = no matches in this batch — a real answer, keep going.
		for _, line := range strings.Split(string(stdout), "\n") {
			if line == "" {
				continue
			}
			// "path:lineno:text" — text may contain ':', split max 3.
			parts := strings.SplitN(line, ":", 3)
			if len(parts) != 3 {
				continue
			}
			taskID, ok := byPath[parts[0]]
			if !ok {
				continue
			}
			n, _ := strconv.Atoi(parts[1])
			out = append(out, LogMatch{TaskID: taskID, Line: n, Text: logfile.StripANSI(parts[2])})
			if len(out) >= maxMatches {
				break
			}
		}
	}
	return out, nil
}

// listTasksFromStore is the shared ListTasks implementation: every lane and
// the Multi router sit on the same store (tasks.target column), so the flat
// table is one SQL query regardless of who serves it.
func listTasksFromStore(ctx context.Context, st *store.Store, opts TaskListOptions) ([]TaskView, int, error) {
	filter := store.TaskFilter{
		JobID:  opts.JobID,
		Status: opts.Status,
		Target: opts.Target,
		Limit:  opts.Limit,
		Offset: opts.Offset,
	}
	rows, err := st.ListTasks(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	total, err := st.CountTasks(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	views := make([]TaskView, 0, len(rows))
	for _, r := range rows {
		views = append(views, BuildTaskView(r))
	}
	return views, total, nil
}

func BuildTaskView(task store.TaskRow) TaskView {
	params := decodeParams(task.ParamsJSON)
	artifacts := readTaskArtifacts(task.TaskDir)
	view := TaskView{
		ID:            task.ID,
		Status:        task.Status,
		Params:        params,
		Metrics:       artifacts.Metrics,
		CurrentStep:   artifacts.CurrentStep,
		ExitCode:      artifacts.ExitCode,
		RetryCount:    task.RetryCount,
		MaxRetry:      task.MaxRetry,
		GPUs:          task.GPUs,
		WandbRunID:    artifacts.WandbRunID,
		ExternalID:    task.ExternalID,
		StatusSource:  task.StatusSource,
		FailureDetail: task.FailureDetail,
		NativeState:   task.NativeState,
		Queue:         task.Queue,
		LogPath:       task.LogPath,
	}
	if task.StartedAt != nil {
		sec := task.StartedAt.Unix()
		view.StartedAt = &sec
	}
	if task.FinishedAt != nil {
		sec := task.FinishedAt.Unix()
		view.FinishedAt = &sec
	}
	if task.StartedAt != nil {
		end := time.Now()
		if task.FinishedAt != nil {
			end = *task.FinishedAt
		}
		elapsed := end.Sub(*task.StartedAt).Seconds()
		if elapsed >= 0 {
			view.ElapsedSec = &elapsed
		}
	}
	return view
}

// BuildDryRunResult expands tasks and adds best-effort preview info.
// getProject is the backend-specific project lookup.
// BuildDryRunResult compiles the cheap plan view. wsRootFor overrides the
// workspace-root preview with the OWNING backend's decision (RQ-65) — nil
// falls back to the local global-config resolution, which is only correct
// for local targets.
func BuildDryRunResult(cfg job.JobConfig, getProject func(string) (*project.Config, error), wsRootFor func(*project.Config) string) (*DryRunResult, error) {
	tasks, err := job.Expand(&cfg)
	if err != nil {
		return nil, err
	}
	result := &DryRunResult{Tasks: tasks}
	if cfg.Project != "" && len(tasks) > 0 {
		if proj, perr := getProject(cfg.Project); perr == nil {
			if proj.CmdTemplate != "" {
				if cmd, rerr := job.Render(proj.CmdTemplate, tasks[0]); rerr == nil {
					result.SampleCommand = cmd
				}
			}
			if wsRootFor != nil {
				result.WorkspaceRoot = wsRootFor(proj)
			} else if storageCfg, cerr := config.Load(); cerr == nil {
				result.WorkspaceRoot = config.ProspectiveRoot(storageCfg, proj.WorkingDir, proj.ProjectName)
			}
		}
	}
	return result, nil
}

// ---- helpers ----

func WandbBaseURL(entity, project string) string {
	if entity != "" {
		return "https://wandb.ai/" + url.PathEscape(entity) + "/" + url.PathEscape(project)
	}
	return "https://wandb.ai/" + url.PathEscape(project)
}

func decodeParams(raw string) map[string]any {
	params := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &params)
	}
	return params
}

type taskArtifacts struct {
	CurrentStep *int
	ExitCode    *int
	WandbRunID  string
	Metrics     map[string]float64
}

func readTaskArtifacts(taskDir string) taskArtifacts {
	var out taskArtifacts
	if taskDir == "" {
		return out
	}
	readMetricsArtifacts(workspace.MetricsPath(taskDir), &out)
	readStatusArtifacts(filepath.Join(taskDir, "status.json"), &out)
	return out
}

func readMetricsArtifacts(path string, out *taskArtifacts) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		key, _ := event["key"].(string)
		if step, ok := numericInt(event["step"]); ok {
			out.CurrentStep = &step
		}
		if key == "_wandb_run_id" {
			if value, ok := event["value"].(string); ok {
				out.WandbRunID = value
			} else if value, ok := event["run_id"].(string); ok {
				out.WandbRunID = value
			}
			continue
		}
		if key != "" && !strings.HasPrefix(key, "_") {
			if value, ok := numericFloat(event["value"]); ok {
				if out.Metrics == nil {
					out.Metrics = map[string]float64{}
				}
				out.Metrics[key] = value
			}
		}
	}
}

func readStatusArtifacts(path string, out *taskArtifacts) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var status struct {
		ExitCode *int `json:"exit_code"`
	}
	if err := json.Unmarshal(buf, &status); err == nil {
		out.ExitCode = status.ExitCode
	}
}

func numericInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		i, err := strconv.Atoi(v.String())
		return i, err == nil
	default:
		return 0, false
	}
}

func numericFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := strconv.ParseFloat(v.String(), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// TaskRowToSchedulerTask converts a store.TaskRow to a scheduler.Task.
// Maps all fields including status, PID, GPUs, and timestamps.
// JSON fields (params, env) are decoded back to maps.
func TaskRowToSchedulerTask(row *store.TaskRow) *scheduler.Task {
	var params map[string]any
	if row.ParamsJSON != "" {
		_ = json.Unmarshal([]byte(row.ParamsJSON), &params)
	}
	var env map[string]string
	if row.EnvJSON != "" {
		_ = json.Unmarshal([]byte(row.EnvJSON), &env)
	}
	// CheckpointDir is derived from TaskDir rather than stored separately —
	// keeps the schema simple, and the value never diverges from
	// RUNQ_CHECKPOINT_DIR (computed the same way in buildTaskEnv).
	var ckptDir string
	if row.TaskDir != "" {
		ckptDir = filepath.Join(row.TaskDir, "checkpoints")
	}
	return &scheduler.Task{
		ID:            row.ID,
		JobID:         row.JobID,
		ProjectName:   row.ProjectName,
		Command:       row.Command,
		Params:        params,
		GPUsNeeded:    row.GPUsNeeded,
		GPUs:          parseGPUIndices(row.GPUs),
		Status:        mapTaskStatus(row.Status),
		RetryCount:    row.RetryCount,
		MaxRetry:      row.MaxRetry,
		PID:           row.PID,
		StartTime:     time.Unix(row.StartTime, 0),
		LogPath:       row.LogPath,
		WorkingDir:    row.WorkingDir,
		Env:           env,
		EnqueuedAt:    row.EnqueuedAt,
		StartedAt:     row.StartedAt,
		FinishedAt:    row.FinishedAt,
		Resumable:     row.Resumable,
		ExtraArgs:     row.ExtraArgs,
		Timeout:       row.Timeout,
		UID:           row.UID,
		TaskDir:       row.TaskDir,
		CheckpointDir: ckptDir,
		ExternalID:    row.ExternalID,
	}
}

// mapTaskStatus converts a DB status string to scheduler.TaskStatus.
func mapTaskStatus(s string) scheduler.TaskStatus {
	switch s {
	case "running":
		return scheduler.StatusRunning
	case "success":
		return scheduler.StatusSuccess
	case "failed":
		return scheduler.StatusFailed
	case "killed":
		return scheduler.StatusKilled
	default:
		return scheduler.StatusPending
	}
}

// parseGPUIndices converts a comma-separated GPU string (e.g. "0,1,3") to []int.
func parseGPUIndices(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	indices := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		indices = append(indices, v)
	}
	return indices
}

// ── Job activity (RQ2-1 §1) ──────────────────────────────────────────────

// activityMaxPoints bounds the points returned per task. The bound is a
// resolution floor, not a data loss: activity columns are cumulative,
// so stride-sampled rows reproduce EXACTLY what full data rebucketed to
// the coarser interval would show.
const activityMaxPoints = 2000

// activityBatch bounds paths per sh invocation (command-line budget,
// mirrors jobLogSearchViaExec).
const activityBatch = 100

// jobActivityViaExec reads every task's activity.tsv ON THE OWNING SIDE
// (RQ-44: results travel, files don't): one sh loop counts, decimates
// with awk, and prefixes each surviving row with its FILENAME — so a
// two-week run (~20k rows/task) crosses the wire as ~2000 rows instead
// of the full file. Shipping full files over SFTP just to drop 90% of
// the rows daemon-side would waste exactly the transfer the decimation
// exists to save.
func jobActivityViaExec(ctx context.Context, st *store.Store, fsys rfs.FS, jobID string) (*JobActivity, error) {
	if fsys == nil {
		fsys = rfs.NewLocalFS()
	}
	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %q: %w", jobID, ErrNotFound)
	}
	rows, err := st.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}

	out := &JobActivity{Tasks: make([]TaskActivity, 0, len(rows))}
	out.JobStart, out.JobEnd = activityWindow(job, rows)

	byPath := make(map[string]int, len(rows)) // activity path → index in out.Tasks
	paths := make([]string, 0, len(rows))
	for _, r := range rows {
		out.Tasks = append(out.Tasks, TaskActivity{
			TaskID: r.ID, Status: r.Status, BucketMin: 1, Points: []ActivityPoint{},
		})
		if r.TaskDir == "" {
			continue // never started — no workspace, empty points is the answer
		}
		p := workspace.ActivityPath(r.TaskDir)
		byPath[p] = len(out.Tasks) - 1
		paths = append(paths, p)
	}

	for start := 0; start < len(paths); start += activityBatch {
		batch := paths[start:min(start+activityBatch, len(paths))]
		stdout, err := execActivityBatch(ctx, fsys, batch)
		if err != nil {
			return nil, err // transport-level: no fact learned about any file
		}
		parseActivityOutput(stdout, byPath, out.Tasks)
	}
	return out, nil
}

// activityWindow derives the job's wall-clock window: start = earliest
// task start (falling back to job creation), end = latest task finish
// once the job is terminal, nil while it is live (the frontend draws
// the axis to now).
func activityWindow(job *store.JobRow, rows []store.TaskRow) (int64, *int64) {
	var earliest, end *int64
	for _, r := range rows {
		if r.StartedAt != nil {
			ts := r.StartedAt.Unix()
			if earliest == nil || ts < *earliest {
				earliest = &ts
			}
		}
		if r.FinishedAt != nil {
			ts := r.FinishedAt.Unix()
			if end == nil || ts > *end {
				end = &ts
			}
		}
	}
	start := job.CreatedAt.Unix()
	if earliest != nil {
		start = *earliest
	}
	if !store.IsTerminalJobStatus(job.Status) {
		return start, nil
	}
	if end == nil && job.FinishedAt != nil {
		ts := job.FinishedAt.Unix()
		end = &ts
	}
	return start, end
}

// execActivityBatch runs the count-then-decimate loop for one batch of
// activity files. Per file: k = ceil(rows/activityMaxPoints); awk keeps
// row 1, every k-th row, and the last row. A `__k` marker line reports
// the stride so the caller can label the resolution. Missing files are
// skipped silently (pending tasks, pre-sidecar runs).
func execActivityBatch(ctx context.Context, fsys rfs.FS, batch []string) (string, error) {
	var sb strings.Builder
	for _, p := range batch {
		q := utils.ShellQuote(p)
		fmt.Fprintf(&sb,
			`if [ -f %s ]; then n=$(wc -l < %s 2>/dev/null || echo 0); k=$(( (n + %d) / %d )); [ "$k" -ge 1 ] || k=1; printf '%%s\t__k\t%%s\n' %s "$k"; awk -v k="$k" 'NR==1 || NR%%k==0 {print FILENAME "\t" $0; l=NR} END {if (NR>0 && NR!=l) print FILENAME "\t" $0}' %s; fi; `,
			q, q, activityMaxPoints-1, activityMaxPoints, q, q)
	}
	stdout, _, _, err := fsys.Exec(ctx, "sh", "-c", sb.String())
	if err != nil {
		return "", fmt.Errorf("job activity: %w", err)
	}
	// Non-zero exit codes are unremarkable: every file access is guarded
	// in-script, and a missing file is an answer, not an error.
	return string(stdout), nil
}

// parseActivityOutput folds decimated rows back into their tasks.
// Line grammar: "<path>\t__k\t<stride>" or "<path>\t<ts>\t<bytes>[\t<lines>]".
// Legacy 2-column rows yield Lines == nil (bytes cannot stand in for
// line counts). Malformed lines are dropped, not fatal.
func parseActivityOutput(stdout string, byPath map[string]int, tasks []TaskActivity) {
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		path, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		idx, ok := byPath[path]
		if !ok {
			continue
		}
		if kstr, isK := strings.CutPrefix(rest, "__k\t"); isK {
			if k, err := strconv.Atoi(strings.TrimSpace(kstr)); err == nil && k > 0 {
				tasks[idx].BucketMin = k // rows are 60s apart → k == minutes/point
			}
			continue
		}
		f := strings.Split(rest, "\t")
		if len(f) < 2 {
			continue
		}
		ts, err1 := strconv.ParseInt(strings.TrimSpace(f[0]), 10, 64)
		bytesV, err2 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		pt := ActivityPoint{TS: ts, Bytes: bytesV}
		if len(f) >= 3 {
			if lv, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64); err == nil {
				pt.Lines = &lv
			}
		}
		tasks[idx].Points = append(tasks[idx].Points, pt)
	}
}
