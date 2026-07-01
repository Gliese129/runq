package backend

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/config"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
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

func BuildJobDetail(job store.JobRow, tasks []store.TaskRow) JobDetail {
	views := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, BuildTaskView(task))
	}
	return JobDetail{
		Job:        BuildJobSummary(job, tasks),
		Tasks:      views,
		MetricKeys: collectMetricKeys(tasks),
		// Raw config (note template, sweep blocks) — powers "re-run as
		// template" in the GUI without a second endpoint.
		Config: json.RawMessage(job.ConfigJSON),
	}
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

func BuildCompareRows(tasks []store.TaskRow, key string, desc bool) []CompareRow {
	rows := make([]CompareRow, 0, len(tasks))
	for _, task := range tasks {
		best, ok := bestMetric(task.TaskDir, key, desc)
		if !ok {
			rows = append(rows, CompareRow{
				TaskID:   task.ID,
				Status:   task.Status,
				Params:   decodeParams(task.ParamsJSON),
				HasValue: false,
			})
			continue
		}
		rows = append(rows, CompareRow{
			TaskID:   task.ID,
			Status:   task.Status,
			Params:   decodeParams(task.ParamsJSON),
			Best:     best,
			HasValue: true,
		})
	}
	// Sort: tasks with values first (ranked), then tasks without values.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].HasValue != rows[j].HasValue {
			return rows[i].HasValue
		}
		if !rows[i].HasValue {
			return false
		}
		if desc {
			return rows[i].Best > rows[j].Best
		}
		return rows[i].Best < rows[j].Best
	})
	for i := range rows {
		if rows[i].HasValue {
			rows[i].Rank = i + 1
		}
	}
	return rows
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

// collectMetricKeys scans the first few tasks with metrics for unique
// metric key names. Internal keys (prefixed with "_") are excluded.
func collectMetricKeys(tasks []store.TaskRow) []string {
	seen := map[string]bool{}
	scanned := 0
	for _, task := range tasks {
		if task.TaskDir == "" || scanned >= 5 {
			continue
		}
		f, err := os.Open(workspace.MetricsPath(task.TaskDir))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			var event struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Key != "" {
				if !strings.HasPrefix(event.Key, "_") {
					seen[event.Key] = true
				}
			}
		}
		f.Close()
		scanned++
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func bestMetric(taskDir, key string, desc bool) (float64, bool) {
	if taskDir == "" || key == "" {
		return 0, false
	}
	f, err := os.Open(workspace.MetricsPath(taskDir))
	if err != nil {
		return 0, false
	}
	defer f.Close()

	var best float64
	has := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var event struct {
			Key   string  `json:"key"`
			Value float64 `json:"value"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Key != key {
			continue
		}
		if !has || (desc && event.Value > best) || (!desc && event.Value < best) {
			best = event.Value
			has = true
		}
	}
	return best, has
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

// ReadMetricPoints reads metrics.jsonl and returns all metric points (excluding internal keys).
func ReadMetricPoints(taskDir string) []MetricPoint {
	if taskDir == "" {
		return nil
	}
	f, err := os.Open(workspace.MetricsPath(taskDir))
	if err != nil {
		return nil
	}
	defer f.Close()

	var points []MetricPoint
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var event struct {
			Key   string  `json:"key"`
			Value float64 `json:"value"`
			Step  *int    `json:"step"`
			TS    int64   `json:"ts"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.Key == "" {
			continue
		}
		if strings.HasPrefix(event.Key, "_") {
			continue
		}
		points = append(points, MetricPoint{
			Key:   event.Key,
			Value: event.Value,
			Step:  event.Step,
			TS:    event.TS,
		})
	}
	return points
}
