package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/store"
	"github.com/gliese129/runq/internal/workspace"
)

var ErrNotSupported = errors.New("not supported")

// Backend is the uniform interface consumed by the dashboard HTTP server
// and CLI --json. Both daemon and HPC backends implement it.
type Backend interface {
	ListJobs(ctx context.Context) ([]JobSummary, error)
	GetJob(ctx context.Context, jobID string) (*JobDetail, error)
	CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error)
	GPUStatus(ctx context.Context) ([]GPUSlot, error)

	KillTask(ctx context.Context, taskID string) error
	RetryTask(ctx context.Context, taskID string) error
	KillJob(ctx context.Context, jobID string) error
	PauseJob(ctx context.Context, jobID string) error
	ResumeJob(ctx context.Context, jobID string) error

	SubmitJob(ctx context.Context, cfg job.JobConfig) (jobID string, totalTasks int, err error)
	DryRun(ctx context.Context, cfg job.JobConfig) ([]job.TaskParams, error)
	GetProject(ctx context.Context, name string) (*project.Config, error)
	ListProjects(ctx context.Context) ([]ProjectSummary, error)
	MatchProjects(ctx context.Context, dir string) ([]ProjectSummary, error)
	CreateProject(ctx context.Context, cfg project.Config) error
}

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

	return JobSummary{
		ID:        job.ID,
		Project:   job.ProjectName,
		Note:      job.Note,
		Status:    job.Status,
		CreatedAt: job.CreatedAt.Unix(),
		Tasks:     counts,
		ETASec:    eta,
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
	}
}

func BuildTaskView(task store.TaskRow) TaskView {
	params := decodeParams(task.ParamsJSON)
	artifacts := readTaskArtifacts(task.TaskDir)
	view := TaskView{
		ID:          task.ID,
		Status:      task.Status,
		Params:      params,
		CurrentStep: artifacts.CurrentStep,
		ExitCode:    artifacts.ExitCode,
		RetryCount:  task.RetryCount,
		WandbRunID:  artifacts.WandbRunID,
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
			continue
		}
		rows = append(rows, CompareRow{
			TaskID: task.ID,
			Params: decodeParams(task.ParamsJSON),
			Best:   best,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return rows[i].Best > rows[j].Best
		}
		return rows[i].Best < rows[j].Best
	})
	for i := range rows {
		rows[i].Rank = i + 1
	}
	return rows
}

// ---- helpers ----

func wandbBaseURL(entity, project string) string {
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
		if step, ok := numericInt(event["step"]); ok {
			out.CurrentStep = &step
		}
		if fmt.Sprint(event["key"]) == "_wandb_run_id" {
			if value, ok := event["value"].(string); ok {
				out.WandbRunID = value
			} else if value, ok := event["run_id"].(string); ok {
				out.WandbRunID = value
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

func refreshStoreJobStatus(ctx context.Context, st *store.Store, jobID string) error {
	tasks, err := st.ListTasks(ctx, store.TaskFilter{JobID: jobID})
	if err != nil {
		return err
	}
	var pending, running int
	var started bool
	for _, task := range tasks {
		switch task.Status {
		case "pending":
			pending++
		case "running":
			running++
			started = true
		case "success", "failed", "killed":
			started = true
		}
	}
	status := "pending"
	if pending+running == 0 {
		status = "done"
	} else if started {
		status = "running"
	}
	return st.UpdateJobStatus(ctx, jobID, status)
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}
