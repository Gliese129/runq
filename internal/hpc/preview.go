package hpc

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gliese129/runq/internal/hpcconfig"
	"github.com/gliese129/runq/internal/hpccore"
	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/submitplan"
	"github.com/gliese129/runq/internal/utils"
)

// Preview compiles the job and renders what WOULD be submitted — the
// representative run.sh (first task) and its submit command — with zero
// side effects: nothing written, nothing persisted, nothing queued.
// The dry-run contract: preview is truth, disk stays untouched (C5/U1).
func (b *Backend) Preview(ctx context.Context, jobCfg job.JobConfig, proj *project.Config, skipPreflight bool) (string, error) {
	disableLocal := b.Cfg.PreflightLocal != nil && !*b.Cfg.PreflightLocal
	plan, err := submitplan.Build(ctx, jobCfg, proj, submitplan.Deps{
		JobID: utils.GenerateID(),
		IDGen: utils.GenerateID,
		Paths: submitplan.Paths{
			WorkspaceRoot: "<workspace>", // placeholder roots: nothing is written
			LogRoot:       "<workspace>",
		},
		SkipPreflight:         skipPreflight,
		PreflightDisableLocal: disableLocal,
		PreflightScope:        "on this login node",
		SchedulerParams:       hpcconfig.TemplateParamRefs(b.Cfg.SubmitTemplate),
	})
	if err != nil {
		return "", err
	}

	var s strings.Builder
	fmt.Fprintf(&s, "dry-run: %d task(s) would be submitted\n\n", len(plan.Tasks))
	for _, c := range plan.Preflight.Results {
		mark := map[string]string{"passed": "✓", "failed": "✗"}[c.Status]
		if mark == "" {
			mark = "-"
		}
		fmt.Fprintf(&s, "%s %-10s %s\n", mark, c.Name, c.Detail)
	}

	t := plan.Tasks[0]
	runsh := filepath.Join(t.TaskDir, runScriptName)
	vars := map[string]string{
		"run_sh": runsh, "gpus": strconv.Itoa(t.GPUsNeeded),
		"job_id": plan.JobID, "task_id": t.TaskID, "task_dir": t.TaskDir,
	}
	for name, value := range t.Params {
		vars["param."+name] = fmt.Sprintf("%v", value)
	}
	cmd, err := hpccore.Render(b.Cfg.SubmitTemplate, vars)
	if err != nil {
		return "", fmt.Errorf("render submit_template: %w", err)
	}

	fmt.Fprintf(&s, "\n── submit command (task 1 of %d) ──\n%s%s\n", len(plan.Tasks), submitEnvPrefix(proj.Environment), cmd)
	fmt.Fprintf(&s, "\n── run.sh (task 1 of %d) ──\n%s", len(plan.Tasks), b.buildRunScript(t, plan))
	return s.String(), nil
}
