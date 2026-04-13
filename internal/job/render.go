package job

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// placeholderRe matches {{name}} style placeholders in command templates.
var placeholderRe = regexp.MustCompile(`\{\{(\w+)}}`)

// Render takes a command template and TaskParams, returns the final command string.
//
// Two-pass replacement:
//  1. Replace named placeholders {{key}} (key != "args") with param values.
//     A key may appear multiple times in the template (e.g. used in two flags).
//  2. Replace {{args}} with remaining unconsumed params as --key=value pairs (sorted).
//
// Errors:
//   - Named placeholder references a key not in params.
//   - Params left unconsumed and no {{args}} in template.
func Render(template string, params TaskParams) (string, error) {
	consumed := make(map[string]bool)

	// ── Pass 1: named placeholders ──
	var errMissing []string
	result := placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		key := match[2 : len(match)-2] // strip {{ and }}

		if key == "args" {
			return match // leave for pass 2
		}

		val, ok := params[key]
		if !ok {
			errMissing = append(errMissing, key)
			return ""
		}
		consumed[key] = true
		return formatValue(val)
	})

	if len(errMissing) > 0 {
		return "", fmt.Errorf(
			"template references unknown parameter(s): %s\nAvailable: %s",
			strings.Join(errMissing, ", "),
			strings.Join(sortedKeys(params), ", "),
		)
	}

	// ── Pass 2: {{args}} ──
	hasArgs := strings.Contains(result, "{{args}}")
	unconsumed := make([]string, 0)
	for _, key := range sortedKeys(params) {
		if !consumed[key] {
			unconsumed = append(unconsumed, key)
		}
	}

	if !hasArgs && len(unconsumed) > 0 {
		return "", fmt.Errorf(
			"template has no {{args}} placeholder, but %d parameter(s) are not referenced: %s\n"+
				"Hint: add {{args}} to your command_template, or use {{%s}} to reference them explicitly",
			len(unconsumed),
			strings.Join(unconsumed, ", "),
			unconsumed[0],
		)
	}

	parts := make([]string, len(unconsumed))
	for i, key := range unconsumed {
		parts[i] = fmt.Sprintf("--%s=%s", key, formatValue(params[key]))
	}
	argsStr := strings.Join(parts, " ")

	result = strings.ReplaceAll(result, "{{args}}", argsStr)

	// Clean up extra whitespace (e.g. trailing space when {{args}} is empty).
	result = strings.Join(strings.Fields(result), " ")

	return result, nil
}

// formatValue converts a parameter value to its string representation.
//
// Uses %v which handles common types naturally:
//
//	int(32)          → "32"
//	float64(0.001)   → "0.001"
//	string("adam")   → "adam"
//	bool(true)       → "true"
//
// Note: YAML scientific notation like 1e-4 is parsed as float64(0.0001),
// so the original literal form is lost. This is acceptable for v1 since
// most ML frameworks accept both forms via argparse.
func formatValue(v any) string {
	return fmt.Sprintf("%v", v)
}

// sortedKeys returns the keys of a TaskParams map in sorted order.
func sortedKeys(params TaskParams) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
