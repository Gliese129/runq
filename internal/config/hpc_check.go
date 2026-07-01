package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gliese129/runq/internal/utils"
)

// hpcSampleVars builds a representative var set for a template's vocabulary.
func hpcSampleVars(field string) map[string]string {
	samples := map[string]string{
		"run_sh":   "/data/runq/jb01HQZX/tk02ABCD/run.sh",
		"gpus":     "1",
		"job_id":   "jb01HQZX",
		"task_id":  "tk02ABCD",
		"name":     "rq-tk02ABCD",
		"task_dir": "/data/runq/jb01HQZX/tk02ABCD",
		"ext_id":   "1234567",
	}
	vars := make(map[string]string)
	for _, name := range HPCPlaceholders[field] {
		vars[name] = samples[name]
	}
	return vars
}

// hpcAddParamSamples synthesizes a sample for every {{param.x}} reference.
func hpcAddParamSamples(vars map[string]string, tmpl string) {
	for _, m := range hpcParamRefRe.FindAllStringSubmatch(tmpl, -1) {
		key := m[1] // "param.h_rt"
		vars[key] = "<" + strings.TrimPrefix(key, "param.") + ">"
	}
}

// HPCCheckResult is one finding of CheckHPC.
type HPCCheckResult struct {
	Name   string `json:"name"`   // e.g. "submit_template"
	Status string `json:"status"` // ok | fail | skip
	Detail string `json:"detail"` // rendered preview, error, or skip reason
}

// CheckHPC statically validates every HPC template against its placeholder
// vocabulary by rendering it with sample values.
func (tc *TargetConfig) CheckHPC() []HPCCheckResult {
	var results []HPCCheckResult

	renderCheck := func(name, field, tmpl string, required bool) HPCCheckResult {
		if strings.TrimSpace(tmpl) == "" {
			if required {
				return HPCCheckResult{name, "fail", "required"}
			}
			return HPCCheckResult{name, "skip", "not set"}
		}
		vars := hpcSampleVars(field)
		if field == "submit_template" {
			hpcAddParamSamples(vars, tmpl)
		}
		out, err := utils.Render(tmpl, vars)
		if err != nil {
			return HPCCheckResult{name, "fail", fmt.Sprintf("%v (valid: {{%s}} + {{param.*}} on submit_template)", err, strings.Join(HPCPlaceholders[field], "}} {{"))}
		}
		return HPCCheckResult{name, "ok", out}
	}

	results = append(results, renderCheck("submit_template", "submit_template", tc.SubmitTemplate, true))

	if strings.Contains(tc.SubmitTemplate, "{{task_id}}") && !strings.Contains(tc.SubmitTemplate, "{{name}}") {
		results = append(results, HPCCheckResult{"job name", "skip",
			"submit_template uses {{task_id}} but not {{name}} — raw ids can start with a digit (SGE rejects them); prefer -N {{name}} (sanitized, supports project job_name templates)"})
	}

	results = append(results, hpcCheckRegex(tc.SubmitIDRegex))

	results = append(results, renderCheck("kill_template", "kill_template", tc.KillTemplate, true))

	if tc.StatusTemplate == "" {
		results = append(results, HPCCheckResult{"status_template", "skip", "not set — status comes from status.json only"})
	} else {
		results = append(results, renderCheck("status_template", "status_template", tc.StatusTemplate, false))
	}

	if len(tc.StatusParser) == 0 {
		results = append(results, HPCCheckResult{"status_parser", "skip", "not set — raw scheduler output parsed by built-in rules"})
	} else {
		for i, stage := range tc.StatusParser {
			results = append(results, renderCheck(fmt.Sprintf("status_parser[%d]", i), "status_parser", stage, false))
		}
	}

	if tc.StatusListTemplate == "" {
		results = append(results, HPCCheckResult{"status_list_template", "skip", "not set — per-job probing via status_template"})
	} else {
		results = append(results, HPCCheckResult{"status_list_template", "ok", tc.StatusListTemplate})
	}
	if len(tc.StatusListParser) == 0 && tc.StatusListTemplate != "" {
		results = append(results, HPCCheckResult{"status_list_parser", "skip", "not set — raw output parsed as 'ext_id signal' lines"})
	} else {
		for i, stage := range tc.StatusListParser {
			results = append(results, HPCCheckResult{fmt.Sprintf("status_list_parser[%d]", i), "ok", stage})
		}
	}

	return results
}

func hpcCheckRegex(pattern string) HPCCheckResult {
	if strings.TrimSpace(pattern) == "" {
		return HPCCheckResult{"submit_id_regex", "fail", "required"}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return HPCCheckResult{"submit_id_regex", "fail", fmt.Sprintf("not a valid regular expression: %v", err)}
	}
	if n := re.NumSubexp(); n != 1 {
		return HPCCheckResult{"submit_id_regex", "fail", fmt.Sprintf("must contain exactly one capture group for the job id (found %d) — use (?:...) for non-capturing groups", n)}
	}
	return HPCCheckResult{"submit_id_regex", "ok", pattern + " (1 capture group)"}
}
