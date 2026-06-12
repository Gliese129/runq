package job

import "strings"

// DefaultJobNameTemplate is used when neither project.yaml (job_name) nor
// job.yaml (name) configures one. Task IDs are hex and may start with a
// digit — SGE/UGE reject such object names — so the default carries a
// letter prefix.
const DefaultJobNameTemplate = "rq-{{task_id}}"

// RenderJobName resolves {{placeholder}}s in a per-task scheduler job name
// (the -N / --job-name value), then sanitizes the result so the scheduler
// can never reject it.
//
// Vocabulary: any param name (per-task value) plus the builtins the caller
// provides (project, job_id, task_id). Unknown placeholders render empty —
// the name is cosmetic, a typo must not block a submit; sanitization
// collapses the leftover separators.
func RenderJobName(tmpl string, params TaskParams, builtins map[string]string) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = DefaultJobNameTemplate
	}
	out := placeholderRe.ReplaceAllStringFunc(tmpl, func(match string) string {
		key := match[2 : len(match)-2]
		if v, ok := builtins[key]; ok {
			return v
		}
		if v, ok := params[key]; ok {
			return formatValue(v)
		}
		return ""
	})
	return SanitizeJobName(out)
}

// SanitizeJobName makes a string safe as a scheduler job name across
// SLURM/PBS/SGE dialects: only [A-Za-z0-9_.-], no leading digit (SGE:
// "cannot start with a digit"), no leading/trailing separators, capped at
// 64 chars (old PBS truncates anyway; predictable beats rejected).
func SanitizeJobName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-.")
	if out == "" {
		return "rq"
	}
	if c := out[0]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		out = "rq-" + out
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-.")
	}
	return out
}
