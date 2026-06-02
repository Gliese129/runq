package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/gliese129/runq/internal/api"
	"github.com/gliese129/runq/internal/resource"
	"github.com/gliese129/runq/internal/service"
	"github.com/gliese129/runq/internal/store"
)

type DaemonBackend struct {
	client *api.Client
}

func NewDaemonBackend(socketPath string) *DaemonBackend {
	return &DaemonBackend{client: api.NewClient(socketPath)}
}

func (b *DaemonBackend) ListJobs(ctx context.Context) ([]JobSummary, error) {
	var jobs []service.JobSummary
	if err := b.do(ctx, "GET", "/api/jobs", nil, &jobs); err != nil {
		return nil, err
	}
	out := make([]JobSummary, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, summaryFromDaemonList(job))
	}
	return out, nil
}

func (b *DaemonBackend) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	var detail struct {
		Job   *store.JobRow   `json:"job"`
		Tasks []store.TaskRow `json:"tasks"`
	}
	if err := b.do(ctx, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	view := BuildJobDetail(*detail.Job, detail.Tasks)
	return &view, nil
}

func (b *DaemonBackend) CompareMetrics(ctx context.Context, jobID, key string, desc bool) ([]CompareRow, error) {
	rows, err := b.taskRows(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return BuildCompareRows(rows, key, desc), nil
}

func (b *DaemonBackend) EvalMatrix(ctx context.Context, jobID, rowKey, colKey, valueKey string) (*MatrixView, error) {
	rows, err := b.taskRows(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return BuildMatrixView(rows, rowKey, colKey, valueKey), nil
}

func (b *DaemonBackend) GPUStatus(ctx context.Context) ([]GPUSlot, error) {
	var gpus []resource.GPUState
	if err := b.do(ctx, "GET", "/api/gpu", nil, &gpus); err != nil {
		return nil, err
	}
	out := make([]GPUSlot, 0, len(gpus))
	for _, gpu := range gpus {
		out = append(out, GPUSlot{
			Index:       gpu.Index,
			Name:        gpu.Name,
			MemTotalMB:  gpu.MemTotal,
			MemUsedMB:   gpu.MemTotal - gpu.MemFree,
			UtilPercent: gpu.UtilPct,
			TaskID:      gpu.TaskID,
			// JobID: requires task→job lookup; left empty for now.
		})
	}
	return out, nil
}

func (b *DaemonBackend) KillTask(ctx context.Context, taskID string) error {
	return b.do(ctx, "POST", "/api/tasks/"+taskID+"/kill", nil, nil)
}

func (b *DaemonBackend) RetryTask(ctx context.Context, taskID string) error {
	return b.do(ctx, "POST", "/api/tasks/"+taskID+"/retry", nil, nil)
}

func (b *DaemonBackend) KillJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "DELETE", "/api/jobs/"+jobID, nil, nil)
}

func (b *DaemonBackend) PauseJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "POST", "/api/jobs/"+jobID+"/pause", nil, nil)
}

func (b *DaemonBackend) ResumeJob(ctx context.Context, jobID string) error {
	return b.do(ctx, "POST", "/api/jobs/"+jobID+"/resume", nil, nil)
}

func (b *DaemonBackend) taskRows(ctx context.Context, jobID string) ([]store.TaskRow, error) {
	var detail struct {
		Job   *store.JobRow   `json:"job"`
		Tasks []store.TaskRow `json:"tasks"`
	}
	if err := b.do(ctx, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		return nil, err
	}
	if detail.Job == nil {
		return nil, fmt.Errorf("job %q not found", jobID)
	}
	return detail.Tasks, nil
}

func (b *DaemonBackend) do(ctx context.Context, method, path string, body any, out any) error {
	_ = ctx
	resp, err := b.client.Do(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error == "" {
			errResp.Error = resp.Status
		}
		return fmt.Errorf("%s", errResp.Error)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func summaryFromDaemonList(job service.JobSummary) JobSummary {
	counts := TaskCountGroup{
		Total:     job.TotalTasks,
		Pending:   job.StatusCount["pending"],
		Running:   job.StatusCount["running"],
		Completed: job.StatusCount["success"],
		Failed:    job.StatusCount["failed"] + job.StatusCount["killed"],
	}
	return JobSummary{
		ID:        job.JobID,
		Project:   job.Project,
		Status:    job.Status,
		CreatedAt: job.CreatedAt.Unix(),
		Tasks:     counts,
	}
}
