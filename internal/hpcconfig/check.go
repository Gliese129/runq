package hpcconfig

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gliese129/runq/internal/hpccore"
)

// Placeholders is the single source of truth for which {{vars}} each
// template accepts. Consumed by `runq hpc config check`, and exposed to the
// dashboard so the GUI template editor's completion can never drift from
// the backend contract.
//
// submit_template additionally accepts the dynamic param.* namespace: every
// task param is exposed as {{param.<name>}} at submit time, so per-task
// scheduler knobs (walltime, queue, priority) can live in the sweep. Check
// cannot know param names statically — it synthesizes "<name>" samples.
var Placeholders = map[string][]string{
	"submit_template": {"run_sh", "gpus", "job_id", "task_id", "task_dir", "name"},
	"kill_template":   {"ext_id"},
	"status_template": {"ext_id"},
	"status_parser":   {"ext_id"},
}

// paramRefRe finds {{param.x}} references for sample synthesis in Check.
var paramRefRe = regexp.MustCompile(`\{\{\s*(param\.[\w\-]+)\s*}}`)

// sampleVars builds a representative var set for a template's vocabulary,
// so rendering doubles as an unknown-placeholder check (Render fails closed).
func sampleVars(field string) map[string]string {
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
	for _, name := range Placeholders[field] {
		vars[name] = samples[name]
	}
	return vars
}

// TemplateParamRefs returns the param names referenced as {{param.X}} in a
// template — these are consumed by the SCHEDULER layer, so the command
// renderer must not demand they appear in command_template too.
func TemplateParamRefs(tmpl string) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range paramRefRe.FindAllStringSubmatch(tmpl, -1) {
		n := strings.TrimPrefix(m[1], "param.")
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// addParamSamples synthesizes a sample for every {{param.x}} reference in
// the template (only submit_template gets the namespace at render time).
func addParamSamples(vars map[string]string, tmpl string) {
	for _, m := range paramRefRe.FindAllStringSubmatch(tmpl, -1) {
		key := m[1] // "param.h_rt"
		vars[key] = "<" + strings.TrimPrefix(key, "param.") + ">"
	}
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
	// required: an empty template is its OWN failure, attributed to its own
	// row (never smeared onto a neighbour by a lumped validate error).
	renderCheck := func(name, field, tmpl string, required bool) CheckResult {
		if strings.TrimSpace(tmpl) == "" {
			if required {
				return CheckResult{name, "fail", "required"}
			}
			return CheckResult{name, "skip", "not set"}
		}
		vars := sampleVars(field)
		if field == "submit_template" {
			addParamSamples(vars, tmpl)
		}
		out, err := hpccore.Render(tmpl, vars)
		if err != nil {
			return CheckResult{name, "fail", fmt.Sprintf("%v (valid: {{%s}} + {{param.*}} on submit_template)", err, strings.Join(Placeholders[field], "}} {{"))}
		}
		return CheckResult{name, "ok", out}
	}

	results = append(results, renderCheck("submit_template", "submit_template", c.SubmitTemplate, true))
	// Advisory: configs predating {{name}} pass raw ids to -N/--job-name —
	// SGE rejects digit-first object names. Not a failure (the dialect is
	// the user's), but worth a nudge toward the sanitized name.
	if strings.Contains(c.SubmitTemplate, "{{task_id}}") && !strings.Contains(c.SubmitTemplate, "{{name}}") {
		results = append(results, CheckResult{"job name", "skip",
			"submit_template uses {{task_id}} but not {{name}} — raw ids can start with a digit (SGE rejects them); prefer -N {{name}} (sanitized, supports project job_name templates)"})
	}

	results = append(results, checkRegex(c.SubmitIDRegex))

	results = append(results, renderCheck("kill_template", "kill_template", c.KillTemplate, true))

	if c.StatusTemplate == "" {
		results = append(results, CheckResult{"status_template", "skip", "not set — status comes from status.json only"})
	} else {
		results = append(results, renderCheck("status_template", "status_template", c.StatusTemplate, false))
	}

	if len(c.StatusParser) == 0 {
		results = append(results, CheckResult{"status_parser", "skip", "not set — raw scheduler output parsed by built-in rules"})
	} else {
		for i, stage := range c.StatusParser {
			results = append(results, renderCheck(fmt.Sprintf("status_parser[%d]", i), "status_parser", stage, false))
		}
	}

	return results
}

// checkRegex validates submit_id_regex on its own row.
func checkRegex(pattern string) CheckResult {
	if strings.TrimSpace(pattern) == "" {
		return CheckResult{"submit_id_regex", "fail", "required"}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return CheckResult{"submit_id_regex", "fail", fmt.Sprintf("not a valid regular expression: %v", err)}
	}
	if n := re.NumSubexp(); n != 1 {
		return CheckResult{"submit_id_regex", "fail", fmt.Sprintf("must contain exactly one capture group for the job id (found %d) — use (?:...) for non-capturing groups", n)}
	}
	return CheckResult{"submit_id_regex", "ok", pattern + " (1 capture group)"}
}
