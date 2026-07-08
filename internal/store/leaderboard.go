package store

import (
	"context"
	"encoding/json"
	"sort"
)

// TaskScore is one task's reduced score for a metric key, used by best/collect.
// Params is the decoded sweep parameter map so callers (CLI --json, web UI) can
// show which hyperparameters produced the value.
type TaskScore struct {
	TaskID   string         `json:"task_id"`
	Status   string         `json:"status"`
	Source   string         `json:"status_source"` // provenance of Status (wrapper/scheduler/inferred/...)
	Params   map[string]any `json:"params"`
	Value    float64        `json:"value"`
	HasValue bool           `json:"has_value"`
}

// MetricLeaderboard reduces every task in a job to its best value for the given
// metric key, then sorts best-first (tasks with no such metric sink to the
// bottom). "Best" means max when maximize is true, else min — applied both when
// reducing a task's steps and when ordering tasks.
//
// This is a shared read query: the HPC CLI uses it for `hpc best` / `hpc
// collect`, and the daemon side can reuse it unchanged.
func (s *Store) MetricLeaderboard(ctx context.Context, jobID, key string, maximize bool) ([]TaskScore, error) {
	tasks, err := s.ListTasks(ctx, TaskFilter{JobID: jobID})
	if err != nil {
		return nil, err
	}

	// Per-task best = summary lookup: the streaming reduction keeps
	// min/max exact, so no point scans — O(tasks) rows total.
	summaries, err := s.ListMetricSummaries(ctx, jobID, key)
	if err != nil {
		return nil, err
	}
	byTask := make(map[string]MetricSummaryRow, len(summaries))
	for _, sm := range summaries {
		byTask[sm.TaskID] = sm
	}

	scores := make([]TaskScore, 0, len(tasks))
	for _, tk := range tasks {
		sc := TaskScore{TaskID: tk.ID, Status: tk.Status, Source: tk.StatusSource}
		_ = json.Unmarshal([]byte(tk.ParamsJSON), &sc.Params)
		if sm, ok := byTask[tk.ID]; ok && sm.Count > 0 {
			sc.HasValue = true
			if maximize {
				sc.Value = sm.Max
			} else {
				sc.Value = sm.Min
			}
		}
		scores = append(scores, sc)
	}

	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].HasValue != scores[j].HasValue {
			return scores[i].HasValue // scored tasks first
		}
		if !scores[i].HasValue {
			return false
		}
		if maximize {
			return scores[i].Value > scores[j].Value
		}
		return scores[i].Value < scores[j].Value
	})
	return scores, nil
}
