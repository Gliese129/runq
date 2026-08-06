package utils

import (
	"sort"
	"strings"
)

// EnvPrelude is THE environment-assembly point for every shell runq
// generates on a target (Codex r2 #2): run.sh, the preflight probe, the
// setup_command wrapper, and the scheduler submit command all render
// their environment through this one struct — so "preflight checked a
// different world than the task ran in" is impossible by construction.
//
// Fixed order (each part optional):
//
//	export HOME=<abs>       — restore context: schedulers strip HOME
//	<env_setup fragment>    — user's declared bootstrapping (module
//	                          load / conda path); runs with HOME right,
//	                          so its ~ expands natively
//	. <env_file> (set -a)   — ambient .env, weakest precedence
//	export K=V ...          — explicit env, always wins
type EnvPrelude struct {
	Home     string            // "" = omit the HOME export
	EnvSetup string            // free-text shell fragment; "" = omit
	EnvFile  string            // path sourced with set -a; "" = omit
	Env      map[string]string // exported sorted (RUNQ_ENV_FILE included verbatim)
}

// Render emits the prelude as newline-terminated shell lines ("" when
// there is nothing to emit).
func (e EnvPrelude) Render() string {
	var b strings.Builder
	if e.Home != "" {
		b.WriteString("export HOME=" + ShellQuote(e.Home) + "\n")
	}
	if s := strings.TrimSpace(e.EnvSetup); s != "" {
		b.WriteString(s + "\n")
	}
	if e.EnvFile != "" {
		q := ShellQuote(e.EnvFile)
		b.WriteString("if [ -f " + q + " ]; then set -a; . " + q + "; set +a; fi\n")
	}
	keys := make([]string, 0, len(e.Env))
	for k := range e.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + ShellQuote(e.Env[k]) + "\n")
	}
	return b.String()
}
