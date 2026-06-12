package job

import (
	"fmt"
	"strings"
)

// RenderSetup fills {{placeholder}}s in a setup_command from fixed params
// ONLY. Setup runs once per job, so a swept param has no single value to
// offer — referencing one is an error, not a guess. Unlike Render (command
// templates), unreferenced params are fine and there is no {{args}}.
func RenderSetup(template string, cfg *JobConfig) (string, error) {
	if !strings.Contains(template, "{{") {
		return template, nil
	}

	swept := make(map[string]bool)
	for _, block := range cfg.Sweep {
		for name := range block.Parameters {
			swept[name] = true
		}
	}

	var bad []string
	out := placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		key := match[2 : len(match)-2]
		if swept[key] {
			bad = append(bad, key+" (swept — setup runs once per job)")
			return ""
		}
		if v, ok := cfg.FixedParams[key]; ok {
			return formatValue(v)
		}
		bad = append(bad, key)
		return ""
	})
	if len(bad) > 0 {
		return "", fmt.Errorf("setup_command references unusable placeholder(s): %s", strings.Join(bad, ", "))
	}
	return out, nil
}
