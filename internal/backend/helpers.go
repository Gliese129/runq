package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/scheduler"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

// ---- builders: store rows → view types ----

func BuildJobSummary(job store.JobRow, tasks []store.TaskRow) JobSummary {
	counts := TaskCountGroup{Total: len(tasks)}
	var finishedDurations []float64
	for _, task := range tasks {
		switch task.Status {
		case "pending":
			counts.Pending++
		case "running":
			counts.Running++
		case "success":
			counts.Completed++
		case "failed", "killed":
			counts.Failed++
		}
		if task.StartedAt != nil && task.FinishedAt != nil && task.FinishedAt.After(*task.StartedAt) {
			finishedDurations = append(finishedDurations, task.FinishedAt.Sub(*task.StartedAt).Seconds())
		}
	}

	var eta *int64
	remaining := counts.Pending + counts.Running
	if len(finishedDurations) > 0 && remaining > 0 {
		var total float64
		for _, d := range finishedDurations {
			total += d
		}
		concurrency := counts.Running
		if concurrency == 0 {
			concurrency = 1
		}
		sec := int64((total / float64(len(finishedDurations))) * float64(remaining) / float64(concurrency))
		eta = &sec
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
		ETASec:      eta,
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
		Config: json.RawMessage(job.ConfigJSON),
	}
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
		ID:           task.ID,
		Status:       task.Status,
		Params:       params,
		Metrics:      artifacts.Metrics,
		CurrentStep:  artifacts.CurrentStep,
		ExitCode:     artifacts.ExitCode,
		RetryCount:   task.RetryCount,
		MaxRetry:     task.MaxRetry,
		GPUs:         task.GPUs,
		WandbRunID:   artifacts.WandbRunID,
		ExternalID:   task.ExternalID,
		StatusSource: task.StatusSource,
		NativeState:  task.NativeState,
		Queue:        task.Queue,
		LogPath:      task.LogPath,
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
func BuildDryRunResult(cfg job.JobConfig, getProject func(string) (*project.Config, error)) (*DryRunResult, error) {
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
			if storageCfg, cerr := config.Load(); cerr == nil {
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
