package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gliese129/runq/internal/backend"
	"github.com/gliese129/runq/internal/utils"
)

func printDashboardJobs(jobs []backend.JobSummary) error {
	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return nil
	}
	w := newTable()
	fmt.Fprintf(w, "JOB_ID\tPROJECT\tSTATUS\tRUN\tPEND\tFAIL\tOK\tETA\n")
	for _, job := range jobs {
		eta := "-"
		if job.ETASec != nil {
			eta = fmt.Sprintf("%ds", *job.ETASec)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			utils.IDColor(job.ID), job.Project, utils.StatusColor(job.Status),
			job.Tasks.Running, job.Tasks.Pending, job.Tasks.Failed, job.Tasks.Completed, eta)
	}
	return w.Flush()
}

func printDashboardDetail(detail *backend.JobDetail) error {
	fmt.Printf("job %s  project=%s  status=%s  tasks=%d\n\n",
		utils.IDColor(detail.Job.ID), detail.Job.Project,
		utils.StatusColor(detail.Job.Status), detail.Job.Tasks.Total)
	if len(detail.Tasks) == 0 {
		return nil
	}

	// Detect HPC fields: show extra columns only when at least one task
	// has an external_id (poll-model backend).
	hasHPC := false
	for _, t := range detail.Tasks {
		if t.ExternalID != "" {
			hasHPC = true
			break
		}
	}

	w := newTable()
	if hasHPC {
		fmt.Fprintf(w, "TASK_ID\tEXT_ID\tSTATUS\tSCHED_STATE\tQUEUE\tRETRY\tSTEP\tELAPSED\tPARAMS\n")
	} else {
		fmt.Fprintf(w, "TASK_ID\tSTATUS\tRETRY\tSTEP\tELAPSED\tPARAMS\n")
	}
	for _, task := range detail.Tasks {
		step := "-"
		if task.CurrentStep != nil {
			step = fmt.Sprintf("%d", *task.CurrentStep)
		}
		elapsed := "-"
		if task.ElapsedSec != nil {
			elapsed = fmt.Sprintf("%.0fs", *task.ElapsedSec)
		}
		status := utils.StatusColor(task.Status)
		if hasHPC {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				utils.IDColor(task.ID), task.ExternalID, status,
				task.NativeState, task.Queue,
				task.RetryCount, step, elapsed, compactJSON(task.Params))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
				utils.IDColor(task.ID), status, task.RetryCount,
				step, elapsed, compactJSON(task.Params))
		}
	}
	return w.Flush()
}

func compactJSON(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(string(buf))
}
