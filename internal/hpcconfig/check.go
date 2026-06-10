package hpcconfig

import (
	"fmt"
	"strings"

	"github.com/gliese129/runq/internal/hpccore"
)

// Placeholders is the single source of truth for which {{vars}} each
// template accepts. Consumed by `runq hpc config check`, and exposed to the
// dashboard so the GUI template editor's completion can never drift from
// the backend contract.
var Placeholders = map[string][]string{
	"submit_template": {"run_sh", "gpus", "job_id", "task_id", "task_dir"},
	"kill_template":   {"ext_id"},
	"status_template": {"ext_id"},
	"status_parser":   {"ext_id"},
}

// sampleVars builds a representative var set for a template's vocabulary,
// so rendering doubles as an unknown-placeholder check (Render fails closed).
func sampleVars(field string) map[string]string {
	samples := map[string]string{
		"run_sh":   "/data/runq/jb01HQZX/tk02ABCD/run.sh",
		"gpus":     "1",
		"job_id":   "jb01HQZX",
		"task_id":  "tk02ABCD",
		"task_dir": "/data/runq/jb01HQZX/tk02ABCD",
		"ext_id":   "1234567",
	}
	vars := make(map[string]string)
	for _, name := range Placeholders[field] {
		vars[name] = samples[name]
	}
	return vars
}

// CheckResult is one finding of Check. Status follows the three-state
// preflight convention: "ok" / "fail" / "skip" (skip = not applicable here,
// never a failure).
type CheckResult struct {
	Name   string // e.g. "submit_template"
	Status string // ok | fail | skip
	Detail string // rendered preview, error, or skip reason
}

// Check statically validates every template against its placeholder
// vocabulary by rendering it with sample values. Zero cost, runs anywhere —
// it is the "preflight for the config itself" when bringing up a new cluster.
func (c *Config) Check() []CheckResult {
	var results []CheckResult

	// name = display label, field = vocabulary key in Placeholders.
	renderCheck := func(name, field, tmpl string) CheckResult {
		out, err := hpccore.Render(tmpl, sampleVars(field))
		if err != nil {
			return CheckResult{name, "fail", fmt.Sprintf("%v (valid: {{%s}})", err, strings.Join(Placeholders[field], "}} {{"))}
		}
		return CheckResult{name, "ok", out}
	}

	results = append(results, renderCheck("submit_template", "submit_template", c.SubmitTemplate))

	// submit_id_regex syntax/capture-group rules are enforced by validate();
	// here we only confirm and restate them so check output is complete.
	if err := c.validate(); err != nil {
		results = append(results, CheckResult{"submit_id_regex", "fail", err.Error()})
	} else {
		results = append(results, CheckResult{"submit_id_regex", "ok", c.SubmitIDRegex + " (1 capture group)"})
	}

	results = append(results, renderCheck("kill_template", "kill_template", c.KillTemplate))

	if c.StatusTemplate == "" {
		results = append(results, CheckResult{"status_template", "skip", "not set — status comes from status.json only"})
	} else {
		results = append(results, renderCheck("status_template", "status_template", c.StatusTemplate))
	}

	if len(c.StatusParser) == 0 {
		results = append(results, CheckResult{"status_parser", "skip", "not set — raw scheduler output parsed by built-in rules"})
	} else {
		for i, stage := range c.StatusParser {
			results = append(results, renderCheck(fmt.Sprintf("status_parser[%d]", i), "status_parser", stage))
		}
	}

	return results
}
