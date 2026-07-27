package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/utils"
)

// Preflight is the L2-C step 10 fail-fast check that runs **before** a
// job is persisted + queued. The failure classes caught at submit time,
// instead of after the task waits 2 hours in a queue:
//
//  1. **Imports**: the target script imports a module that the project
//     Python env cannot resolve.
//  2. **Path args**: the rendered command references an absolute path
//     that does not exist on disk.
//  3. **Writability**: working_dir is not writable.
//  4. **HuggingFace repos** (RQ-76): the script references a Hub repo
//     that does not exist / is gated — or is not cached, in which case
//     the compute node will try to download it at run time (legitimate
//     on clusters with internet; fatal on air-gapped ones — surfaced as
//     a warning with a ready-made pre-download command, never a block).
//  5. **Python env**: the interpreter/env activates in a non-login,
//     non-interactive shell — the shell class the compute node gives
//     the task (a conda registered only in ~/.bashrc fails here).
//
// Imports, HF, and env checks run as ONE generated python probe file
// executed in ONE shell (single env activation) — see probe.go. The
// old per-module subprocess design paid a conda activation per import
// and routinely blew request timeouts.
//
// Each check folds into a `CheckResult`; the final `Report` aggregates
// them. Only "failed" entries block submission.
//
// The `Skip` flag lets the CLI bypass the whole thing with
// `runq submit --no-preflight`; users with conditional imports or
// runtime path inference can opt out.
type Preflight struct {
	// Skip disables every check and makes RunPreflight a no-op. Wired
	// to the CLI's --no-preflight flag.
	Skip bool

	// ProbeTimeout caps the single probe execution (env activation +
	// all imports + HF lookups). A timeout is never a failure — checks
	// whose markers did not arrive report as skipped. Default 30s.
	ProbeTimeout time.Duration

	// DisableLocal turns the probe-based checks (imports, hf, env) into
	// skipped results — for clusters whose login-node policy forbids
	// running subprocesses there (hpc: preflight_local: false).
	DisableLocal bool

	// Scope labels where probe checks ran (e.g. "on login node") so HPC
	// users see the verification boundary in passed results.
	Scope string

	// ExcludeParams are scheduler-consumed params (submit_template's
	// {{param.*}}) — the sample command renders without demanding them.
	ExcludeParams map[string]bool

	// Env is the merged task environment (project environment + job
	// overrides, incl. the reserved RUNQ_ENV_FILE) and EnvSetup the
	// target's env_setup fragment: the probe assembles THE SAME
	// environment run.sh gives the task — one injection point, so
	// preflight and run can never disagree about what is exported.
	Env      map[string]string
	EnvSetup string

	// FS is the filesystem THE TASK will run against. nil = local os
	// semantics (current machine). For remote targets this is the target's
	// rfs.FS: path/writability checks stat the REMOTE filesystem, the
	// script is read from it, and the probe executes on the remote
	// login node — checking the client machine would answer a question
	// nobody asked. GPU checks are skipped for remote (login nodes have no
	// GPUs; the probe would only produce noise).
	FS rfs.FS

	// taskParams is the full expanded sweep, set by Run — "{{param.X}}"
	// entries in the project's preflight.hf contract expand against EVERY
	// task's value, not just the sample command's.
	taskParams []job.TaskParams
}

// fsys returns the filesystem checks run against (local when unset).
func (p Preflight) fsys() rfs.FS {
	if p.FS != nil {
		return p.FS
	}
	return rfs.NewLocalFS()
}

// runShell executes a probe command where the task will run: locally via
// bash, remotely via FS.Exec. Returns (combined output, exit code, err).
// err non-nil means the probe COULD NOT RUN (transport/env problem) — no
// fact was learned, and per the no-false-positive discipline that is never
// a finding. A non-zero exit code is the probe's own verdict. Partial
// output is returned in every case (a timed-out probe still yields the
// markers that made it out).
func (p Preflight) runShell(ctx context.Context, cmd string) (string, int, error) {
	if p.FS == nil {
		c := exec.CommandContext(ctx, "bash", "-c", cmd)
		// ProbeTimeout is a WALL-CLOCK contract (Codex r1 #5): killing
		// only the outer bash leaves grandchildren holding the output
		// pipe, and CombinedOutput then blocks until THEY exit. Kill the
		// whole process group on cancel, and cap the post-cancel pipe
		// wait as a backstop.
		setupProcessGroup(c)
		c.WaitDelay = time.Second
		out, err := c.CombinedOutput()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return string(out), ee.ExitCode(), nil
			}
			return string(out), -1, err
		}
		return string(out), 0, nil
	}
	stdout, stderr, code, err := p.FS.Exec(ctx, "sh", "-c", cmd)
	return string(stdout) + string(stderr), code, err
}

// DefaultPreflight returns a preflight runner with sane defaults. 20s
// keeps the whole preflight under the dashboard's 30s request timeout
// even with slow login-node imports (Codex r1 #5: probe timeout must
// undercut the transport timeout, or the browser gives up first).
func DefaultPreflight() Preflight {
	return Preflight{
		ProbeTimeout: 20 * time.Second,
	}
}

// Run executes preflight against a project and expanded task params,
// rendering the sample command internally ("first task is representative").
// Returns the four-state report; report.Err() is the blocking error.
func (p Preflight) Run(ctx context.Context, proj *project.Config, taskParams []job.TaskParams) (Report, error) {
	sampleCmd, err := renderSampleCommand(proj, taskParams, p.ExcludeParams)
	if err != nil {
		return Report{}, err
	}
	p.taskParams = taskParams
	return p.RunPreflight(ctx, proj, sampleCmd), nil
}

// PreflightFinding is one failed check.
type PreflightFinding struct {
	Kind   string // "import" | "path" | "writable" | "hf"
	Detail string
}

func (f PreflightFinding) String() string {
	return fmt.Sprintf("  - %s: %s", f.Kind, f.Detail)
}

// CheckResult is one check in the four-state grammar: passed / failed /
// warning / skipped. skipped means "could not be checked HERE" (missing
// prerequisite, disabled by config, probe timeout); warning means "found
// something, but it correlates too weakly with task runnability to
// block". Only "failed" blocks a submit.
type CheckResult struct {
	Name   string `json:"name"`   // writable | paths | python_env | imports | huggingface | gpu
	Status string `json:"status"` // passed | failed | warning | skipped
	Detail string `json:"detail,omitempty"`
	// Commands are ready-made remediation commands for this result (e.g.
	// `huggingface-cli download org/model` for an uncached Hub repo).
	// The CLI prints them; the WebUI offers one-click insertion into the
	// project's setup command.
	Commands []string `json:"commands,omitempty"`
}

// Report aggregates all check results. Only failed entries block.
type Report struct {
	Results []CheckResult `json:"results"`
	// HomeDir / PythonPrefix are facts the probe learned about the target
	// environment (login node): the absolute $HOME (used by run.sh HOME
	// restoration, RQ-76 ①) and the resolved sys.prefix. Informational.
	HomeDir      string `json:"home_dir,omitempty"`
	PythonPrefix string `json:"python_prefix,omitempty"`
}

func (r Report) OK() bool {
	for _, c := range r.Results {
		if c.Status == "failed" {
			return false
		}
	}
	return true
}

// Err converts failed results to an error for SubmitJob to return.
func (r Report) Err() error {
	if r.OK() {
		return nil
	}
	lines := []string{"preflight failed:"}
	for _, c := range r.Results {
		if c.Status == "failed" {
			lines = append(lines, fmt.Sprintf("  - %s: %s", c.Name, c.Detail))
		}
	}
	lines = append(lines, "", "Run with --no-preflight to skip this check (not recommended).")
	return errors.New(strings.Join(lines, "\n"))
}

// fold collapses a category's findings into one result. scope ("on login
// node") is appended to passed checks — login-node facts do not
// guarantee the compute-node environment, and the report should say so
// rather than imply more than was verified.
func fold(name string, findings []PreflightFinding, scope string) CheckResult {
	if len(findings) > 0 {
		details := make([]string, 0, len(findings))
		for _, f := range findings {
			details = append(details, f.Detail)
		}
		return CheckResult{Name: name, Status: "failed", Detail: strings.Join(details, "; ")}
	}
	detail := "ok"
	if scope != "" {
		detail = "ok (verified " + scope + ")"
	}
	return CheckResult{Name: name, Status: "passed", Detail: detail}
}

// RunPreflight executes all checks against the project config + a
// **sample rendered command** (the first task's command is the natural
// choice — all tasks in a sweep share the same script + same env, only
// the args differ, and path-arg / import checks are independent of which
// concrete arg values are used).
func (p Preflight) RunPreflight(ctx context.Context, proj *project.Config, sampleCmd string) Report {
	if p.Skip {
		return Report{Results: []CheckResult{{Name: "preflight", Status: "skipped", Detail: "disabled by --no-preflight"}}}
	}
	if !proj.Preflight.EnabledOrDefault() {
		return Report{Results: []CheckResult{{Name: "preflight", Status: "skipped", Detail: "disabled by project preflight.enabled"}}}
	}
	var results []CheckResult

	// Tier free: filesystem checks — against the TARGET's filesystem.
	results = append(results, fold("writable", p.checkWritable(proj.WorkingDir, "working_dir"), p.Scope))
	results = append(results, fold("paths", p.checkPathArgs(sampleCmd), p.Scope))

	// Tier probe: one generated python file, one shell, one env
	// activation. Skippable by cluster policy.
	var report Report
	if p.DisableLocal {
		results = append(results,
			CheckResult{Name: "imports", Status: "skipped", Detail: "disabled by hpc preflight_local"},
			CheckResult{Name: "huggingface", Status: "skipped", Detail: "disabled by hpc preflight_local"})
	} else {
		results = append(results, p.probeChecks(ctx, proj, sampleCmd, &report)...)
	}

	// GPU smoke test: probe-don't-enumerate (C2). No driver here ≠ failure.
	// Remote targets skip outright: GPUs live on compute nodes, and probing
	// a login node's nvidia-smi would only produce noise.
	if p.FS != nil {
		results = append(results, CheckResult{Name: "gpu", Status: "skipped", Detail: "remote target: GPUs live on compute nodes"})
	} else {
		results = append(results, checkGPU(ctx, proj, p.Scope))
	}

	report.Results = results
	return report
}

// contractParamRegex matches "{{param.NAME}}" placeholders in declared
// preflight.hf entries.
var contractParamRegex = regexp.MustCompile(`^\{\{param\.(\w+)}}$`)

// expandContractHF turns the declared preflight.hf list into concrete
// refs. "{{param.NAME}}" entries expand against EVERY task's value —
// sweep 8 models and all 8 ids are verified, not just the sample.
// Declared entries are a contract, so malformed ones are hard findings,
// never silently dropped.
func expandContractHF(declared []string, taskParams []job.TaskParams) ([]HFRef, []PreflightFinding) {
	var refs []HFRef
	var findings []PreflightFinding
	seen := map[string]bool{}
	add := func(id, origin string) {
		if !looksLikeHFRepoID(id) {
			findings = append(findings, PreflightFinding{
				Kind:   "hf",
				Detail: fmt.Sprintf("preflight.hf entry %s resolves to %q — not a valid HuggingFace repo id", origin, id),
			})
			return
		}
		if !seen[id] {
			seen[id] = true
			refs = append(refs, HFRef{RepoID: id, RepoType: "any"})
		}
	}
	for _, entry := range declared {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		m := contractParamRegex.FindStringSubmatch(entry)
		if m == nil {
			add(entry, "\""+entry+"\"")
			continue
		}
		name := m[1]
		found := false
		for _, params := range taskParams {
			if v, ok := params[name]; ok {
				found = true
				add(fmt.Sprintf("%v", v), entry)
			}
		}
		if !found {
			findings = append(findings, PreflightFinding{
				Kind:   "hf",
				Detail: fmt.Sprintf("preflight.hf entry %q references a param that no task defines", entry),
			})
		}
	}
	return refs, findings
}

// shellEntryRegex extracts the shell script path from a `bash x.sh` /
// `sh x.sh` / `./x.sh` entry command.
var shellEntryRegex = regexp.MustCompile(`(?:^|\s)(?:bash\s+|sh\s+)?((?:\.?/)?[^\s]+\.sh)\b`)

// shellEntry resolves the entry .sh path (against workingDir) or "".
func shellEntry(sampleCmd, workingDir string) string {
	m := shellEntryRegex.FindStringSubmatch(sampleCmd)
	if len(m) < 2 {
		return ""
	}
	s := m[1]
	if !strings.HasPrefix(s, "/") {
		s = path.Join(workingDir, s)
	}
	return s
}

// probeChecks runs the single-probe tier (env + imports + HF + wandb +
// extra_run contract + shell syntax) and folds the outcome into results.
// Facts about the environment (home, prefix) land on the report.
//
// The contract principle (RQ-76 ②): what project.yaml DECLARES is
// verified exactly and blocks on failure; what is not declared is
// best-effort static analysis of a .py entry — and when there is
// nothing to analyze (shell entry), the report says so and points at
// the contract instead of guessing.
func (p Preflight) probeChecks(ctx context.Context, proj *project.Config, sampleCmd string, report *Report) []CheckResult {
	contract := proj.Preflight

	// Source discovery follows the code the task runs (Codex r1 #2):
	// .py entry directly, .sh entry via the python invocations inside it,
	// then the LOCAL import graph (train → helper → third-party).
	entrySources, shEntryPath, readErr := p.collectEntrySources(sampleCmd, proj.WorkingDir)
	modules, scanned := p.walkImportGraph(entrySources, proj.WorkingDir)

	importsSkip := ""
	switch {
	case !contract.ImportsOrDefault():
		importsSkip, modules = "disabled by preflight.imports", nil
	case readErr != nil:
		importsSkip = fmt.Sprintf("cannot read entry script: %v", readErr)
	case len(entrySources) == 0 && shEntryPath != "":
		importsSkip = "no python invocation found inside the shell entry — declare preflight.hf / preflight.extra_run in project.yaml to verify what it runs"
	case len(entrySources) == 0:
		importsSkip = "no python entry — declare preflight.hf / preflight.extra_run in project.yaml to verify non-python workloads"
	}

	// HF refs: best-effort static extraction over EVERY scanned source
	// (entry + shell-invoked scripts + local deps) + declared contract.
	var refs []HFRef
	seenHF := map[string]bool{}
	for _, s := range scanned {
		for _, r := range ExtractHFRefs(s.Src) {
			key := r.RepoType + ":" + r.RepoID
			if !seenHF[key] {
				seenHF[key] = true
				refs = append(refs, r)
			}
		}
	}
	declaredRefs, hfContractFindings := expandContractHF(contract.HFRepos(), p.taskParams)
	seenRef := map[string]bool{}
	for _, r := range refs {
		seenRef[r.RepoID] = true
	}
	for _, r := range declaredRefs {
		if !seenRef[r.RepoID] {
			refs = append(refs, r)
		}
	}

	wandb := contract.WandbOrDefault()
	extraRun := strings.TrimSpace(contract.ExtraRunOrEmpty())
	shEntry := shEntryPath
	// Interpreter: the entry command's python token, else the one the
	// shell script invokes, else plain "python".
	interp := pythonInterpreter(sampleCmd)
	if interp == "python" && shEntry != "" && len(entrySources) > 0 {
		if shSrc, err := p.fsys().ReadFile(shEntry); err == nil {
			interp = pythonInterpreter(string(shSrc))
		}
	}
	envDeclared := proj.PythonEnv.Type != ""

	// Malformed contract entries block before any probe runs — a contract
	// that cannot be verified must not pass silently.
	if len(hfContractFindings) > 0 {
		return []CheckResult{fold("huggingface", hfContractFindings, p.Scope)}
	}

	needPython := len(modules) > 0 || len(refs) > 0 || wandb || envDeclared
	if !needPython && extraRun == "" && shEntry == "" {
		return []CheckResult{{Name: "imports", Status: "skipped", Detail: importsSkip}}
	}

	timeout := p.ProbeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, code, runErr, writeErr := p.execProbe(cctx, proj, interp, modules, refs, wandb, extraRun, shEntry, needPython)
	if writeErr != nil {
		return []CheckResult{
			{Name: "imports", Status: "skipped", Detail: fmt.Sprintf("cannot write probe file: %v", writeErr)},
			{Name: "huggingface", Status: "skipped", Detail: "probe not run"},
		}
	}

	res := parseProbeOutput(out, refs)
	report.HomeDir = res.Home
	report.PythonPrefix = res.Prefix
	timedOut := cctx.Err() != nil

	var results []CheckResult

	// Shell syntax (bash -n): runs before env activation, so its verdict
	// survives even an activation failure. Zero false positives — the
	// same parser that will run the script parsed it.
	if shEntry != "" && res.ShellSyntaxRC >= 0 {
		if res.ShellSyntaxRC == 0 {
			results = append(results, CheckResult{Name: "shell_syntax", Status: "passed", Detail: "bash -n ok"})
		} else {
			results = append(results, CheckResult{Name: "shell_syntax", Status: "failed",
				Detail: fmt.Sprintf("entry script has shell syntax errors: %s", res.ShellSyntaxDetail)})
		}
	}

	skipRest := func(reason string) []CheckResult {
		if importsSkip == "" {
			importsSkip = reason
		}
		results = append(results, CheckResult{Name: "imports", Status: "skipped", Detail: importsSkip})
		if len(refs) > 0 {
			results = append(results, CheckResult{Name: "huggingface", Status: "skipped", Detail: reason})
		}
		if extraRun != "" {
			results = append(results, CheckResult{Name: "extra_run", Status: "skipped", Detail: reason})
		}
		return results
	}

	if !res.PythonRan && !res.InnerRan {
		switch {
		case timedOut:
			return skipRest(fmt.Sprintf("probe timed out after %s (login node slow?) — nothing verified", timeout))
		case runErr != nil:
			// Transport problem: probe could not run — no fact learned.
			return skipRest(fmt.Sprintf("probe could not run: %v", runErr))
		case code != 0 && envDeclared:
			// The shell ran and failed before python emitted anything:
			// env activation is broken in a non-interactive shell — the
			// exact failure the compute node would hit (RQ-76 ①/feedback:
			// conda registered only in ~/.bashrc).
			results = append(results, CheckResult{Name: "python_env", Status: "failed", Detail: fmt.Sprintf(
				"environment failed to activate in a non-interactive shell: %s — compute nodes use the same shell class (no ~/.bashrc), so the task would fail too; make the env available via env_setup / absolute paths",
				lastLine(out))})
			return skipRest("environment activation failed")
		case code != 0:
			return skipRest(fmt.Sprintf("probe shell failed: %s", lastLine(out)))
		default:
			return skipRest("probe produced no output")
		}
	}

	if !res.PythonRan {
		// The wrapped chain ran (pyexit arrived) but python emitted
		// nothing — interpreter missing or broken.
		if envDeclared {
			results = append(results, CheckResult{Name: "python_env", Status: "failed", Detail: fmt.Sprintf(
				"environment activated but %q did not run (exit %d): %s", interp, res.PyExit, lastLine(out))})
		} else if needPython {
			results = append(results, CheckResult{Name: "python_env", Status: "skipped", Detail: fmt.Sprintf(
				"no usable python on the target (exit %d) — python checks skipped; use preflight.extra_run for shell-level verification", res.PyExit)})
		}
		if importsSkip == "" {
			importsSkip = "python did not run"
		}
		results = append(results, CheckResult{Name: "imports", Status: "skipped", Detail: importsSkip})
		if len(refs) > 0 {
			results = append(results, CheckResult{Name: "huggingface", Status: "skipped", Detail: "python did not run"})
		}
	} else {
		results = append(results, p.envCheck(proj, res.Prefix))
		if importsSkip != "" {
			results = append(results, CheckResult{Name: "imports", Status: "skipped", Detail: importsSkip})
		} else {
			results = append(results, p.importsCheck(modules, res, timedOut, timeout))
		}
		if len(refs) > 0 {
			results = append(results, p.hfCheck(refs, res, timedOut, timeout))
		}
		if wandb {
			switch res.Wandb {
			case "ok":
				results = append(results, CheckResult{Name: "wandb", Status: "passed", Detail: "credentials found (WANDB_API_KEY or ~/.netrc)"})
			case "missing":
				results = append(results, CheckResult{Name: "wandb", Status: "warning", Detail: "no WANDB_API_KEY and no api.wandb.ai entry in ~/.netrc — the task may hang on wandb login"})
			default:
				results = append(results, CheckResult{Name: "wandb", Status: "skipped", Detail: "probe cut short before the wandb check"})
			}
		}
	}

	// extra_run contract: user-authored, runs inside the activated env in
	// the SAME shell. Non-zero exit blocks — the whole point is that the
	// user declared "this must hold before anything is queued".
	if extraRun != "" {
		switch {
		case res.ExtraRC == 0:
			results = append(results, CheckResult{Name: "extra_run", Status: "passed", Detail: "custom check ok"})
		case res.ExtraRC > 0:
			tail := res.ExtraOut
			if len(tail) > 3 {
				tail = tail[len(tail)-3:]
			}
			results = append(results, CheckResult{Name: "extra_run", Status: "failed",
				Detail: fmt.Sprintf("custom check exited %d: %s", res.ExtraRC, strings.Join(tail, " | "))})
		case res.ExtraStarted:
			results = append(results, CheckResult{Name: "extra_run", Status: "skipped",
				Detail: fmt.Sprintf("custom check cut short (probe timeout %s)", timeout)})
		default:
			results = append(results, CheckResult{Name: "extra_run", Status: "skipped", Detail: "custom check did not run"})
		}
	}

	return results
}

// execProbe writes the probe file next to the project (so relative
// resolution matches the task's world) and runs ONE shell:
//
//	[bash -n entry.sh]                      — outside the env (parser only)
//	activate env → python probe → extra_run — one activation for everything
//	rm probe
//
// Marker lines emitted by the shell itself (pyexit, shellsyntax, extra
// begin/end) let the parser attribute failures precisely: activation vs
// interpreter vs the user's custom check.
func (p Preflight) execProbe(ctx context.Context, proj *project.Config, interp string, modules []string, refs []HFRef, wandb bool, extraRun, shEntry string, needPython bool) (out string, code int, runErr, writeErr error) {
	script := buildProbeScript(modules, refs, wandb)
	probePath := path.Join(proj.WorkingDir, fmt.Sprintf(".runq_preflight_%d.py", time.Now().UnixNano()))
	if p.FS == nil {
		writeErr = os.WriteFile(probePath, []byte(script), 0o644)
	} else {
		writeErr = p.FS.WriteFile(probePath, []byte(script), 0o644)
	}
	if writeErr != nil {
		return "", 0, nil, writeErr
	}

	// SAME ENVIRONMENT AS THE TASK (Codex r1 #3): the probe mirrors
	// run.sh's environment assembly — target env_setup, the ambient .env
	// file, then the project/override env exports (which must win). A
	// probe that checks a DIFFERENT environment than the task runs in
	// produces false verdicts (wandb creds exported by run.sh, reported
	// missing by preflight).
	var envPrefix strings.Builder
	if setup := strings.TrimSpace(p.EnvSetup); setup != "" {
		envPrefix.WriteString(setup + "\n")
	}
	if envFile := p.Env["RUNQ_ENV_FILE"]; envFile != "" {
		q := shQuote(envFile)
		envPrefix.WriteString("if [ -f " + q + " ]; then set -a; . " + q + "; set +a; fi\n")
	}
	for _, k := range sortedEnvKeys(p.Env) {
		if k == "RUNQ_ENV_FILE" {
			continue
		}
		envPrefix.WriteString("export " + k + "=" + shQuote(p.Env[k]) + "\n")
	}

	// Inner chain: python probe (+ its exit marker), then the extra_run
	// contract bracketed by markers — all inside ONE env activation.
	inner := interp + " " + shQuote(probePath) + `; printf '` + probeMarker + `\tpyexit\t-\t%s\n' "$?"`
	if extraRun != "" {
		inner += `; printf '` + probeMarker + `\textra\tbegin\t0\n'; { ` + extraRun + `
} ; printf '` + probeMarker + `\textra\tend\t%s\n' "$?"`
	}
	if !needPython {
		// Pure-shell contract: no python work to do; run only extra_run
		// inside the env (activation may still matter to it).
		inner = `printf '` + probeMarker + `\tpyexit\t-\t0\n'`
		if extraRun != "" {
			inner += `; printf '` + probeMarker + `\textra\tbegin\t0\n'; { ` + extraRun + `
} ; printf '` + probeMarker + `\textra\tend\t%s\n' "$?"`
		}
	}
	// Group the inner chain: WrapCommand joins with `&&`, and a bare `;`
	// inside inner would detach the marker printfs from that chain — the
	// pyexit marker would then fire even when ACTIVATION failed, and the
	// parser would misread an activation failure as "python missing".
	inner = "{ " + inner + " ; }"
	wrapped := utils.WrapCommand(
		proj.PythonEnv.Type, proj.PythonEnv.Path, proj.PythonEnv.Name,
		inner,
		proj.WorkingDir,
	)

	var pre string
	if shEntry != "" {
		// bash -n before activation: a syntax verdict must survive an
		// activation failure. Output flattened to one marker line.
		pre = `__pf_out=$(bash -n ` + shQuote(shEntry) + ` 2>&1); printf '` + probeMarker + `\tshellsyntax\tscript\t%s\t%s\n' "$?" "$(printf '%s' "$__pf_out" | tr '\n\t' '  ')"; `
	}

	// Group the wrapped chain so cleanup runs regardless of its verdict,
	// while the probe's own exit code is preserved.
	full := fmt.Sprintf("%s%s{ %s ; }; rc=$?; rm -f %s; exit $rc",
		pre, envPrefix.String(), wrapped, shQuote(probePath))
	out, code, runErr = p.runShell(ctx, full)
	if ctx.Err() != nil {
		// The shell was killed before its own `rm -f` could run — clean
		// the probe file up from THIS side (Codex r1 #5: 100ms-timeout
		// runs left .runq_preflight_*.py behind).
		p.removeProbe(probePath)
	}
	return out, code, runErr, nil
}

// removeProbe best-effort deletes a probe file after a killed run.
func (p Preflight) removeProbe(probePath string) {
	if p.FS == nil {
		_ = os.Remove(probePath)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _, _ = p.FS.Exec(ctx, "rm", "-f", probePath)
}

// sortedEnvKeys returns map keys sorted for deterministic script output.
func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// envCheck verifies the resolved sys.prefix against the declared env.
// The probe RUNNING already proves activation works in a non-interactive
// shell; this only cross-checks that it resolved to the env the user
// meant. Mismatches warn (never block): activation clearly produced a
// working python, and prefix layouts vary across conda installs.
func (p Preflight) envCheck(proj *project.Config, prefix string) CheckResult {
	scoped := func(d string) string {
		if p.Scope != "" {
			return d + " (verified " + p.Scope + ")"
		}
		return d
	}
	if prefix == "" {
		return CheckResult{Name: "python_env", Status: "passed", Detail: scoped("environment activates in a non-interactive shell")}
	}
	switch proj.PythonEnv.Type {
	case "conda":
		name := proj.PythonEnv.Name
		if name != "" && name != "base" {
			if path.Base(prefix) == name || strings.Contains(prefix, "/envs/"+name) {
				return CheckResult{Name: "python_env", Status: "passed", Detail: scoped("conda env " + name + " at " + prefix)}
			}
			return CheckResult{Name: "python_env", Status: "warning", Detail: fmt.Sprintf(
				"python resolves to %s, which does not look like conda env %q — check the env name", prefix, name)}
		}
	case "venv", "uv":
		envPath := proj.PythonEnv.Path
		if envPath == "" {
			envPath = ".venv"
		}
		if !path.IsAbs(envPath) {
			envPath = path.Join(proj.WorkingDir, envPath)
		}
		if path.Clean(prefix) == path.Clean(envPath) {
			return CheckResult{Name: "python_env", Status: "passed", Detail: scoped("venv at " + prefix)}
		}
		return CheckResult{Name: "python_env", Status: "warning", Detail: fmt.Sprintf(
			"python resolves to %s, expected venv %s", prefix, envPath)}
	}
	return CheckResult{Name: "python_env", Status: "passed", Detail: scoped("python at " + prefix)}
}

// importsCheck folds the probe's per-module verdicts. A cut-short probe
// (timeout) skips rather than passes: unverified is not verified.
func (p Preflight) importsCheck(modules []string, res probeOutcome, timedOut bool, timeout time.Duration) CheckResult {
	var findings []PreflightFinding
	for _, imp := range res.Imports {
		if !imp.OK {
			findings = append(findings, PreflightFinding{
				Kind:   "import",
				Detail: fmt.Sprintf("cannot import %q in env: %s", imp.Module, imp.Detail),
			})
		}
	}
	if len(findings) > 0 {
		return fold("imports", findings, p.Scope)
	}
	if len(res.Imports) < len(modules) {
		detail := fmt.Sprintf("checked %d/%d modules, none failed", len(res.Imports), len(modules))
		if timedOut {
			detail = fmt.Sprintf("probe timed out after %s — %s", timeout, detail)
		}
		return CheckResult{Name: "imports", Status: "skipped", Detail: detail}
	}
	if len(modules) == 0 {
		return CheckResult{Name: "imports", Status: "skipped", Detail: "no external imports to check"}
	}
	return fold("imports", nil, p.Scope)
}

// hfCheck folds the probe's Hub verdicts:
//
//   - missing / gated repo → failed (the task cannot possibly load it)
//   - reachable but not cached → warning + pre-download commands: two
//     legitimate worlds exist (already planning to download on the
//     compute node vs air-gapped compute), so runq surfaces instead of
//     blocking — and hands the user the exact command to pin the data
//     down beforehand.
//   - all cached → passed
//   - huggingface_hub missing / network unknown / cut short → skipped
func (p Preflight) hfCheck(refs []HFRef, res probeOutcome, timedOut bool, timeout time.Duration) CheckResult {
	var findings []PreflightFinding
	var uncached []HFRef
	var unknown, nohub, cached int
	for _, h := range res.HF {
		switch h.Status {
		case "cached":
			cached++
		case "reachable":
			uncached = append(uncached, h.Ref)
		case "missing":
			findings = append(findings, PreflightFinding{
				Kind:   "hf",
				Detail: fmt.Sprintf("repo %q not found on the Hub (%s)", h.Ref.RepoID, h.Detail),
			})
		case "gated":
			findings = append(findings, PreflightFinding{
				Kind:   "hf",
				Detail: fmt.Sprintf("repo %q is gated — request access and make the HF token available where the task runs (%s)", h.Ref.RepoID, h.Detail),
			})
		case "nohub":
			nohub++
		default:
			unknown++
		}
	}
	if len(findings) > 0 {
		return fold("huggingface", findings, p.Scope)
	}
	if nohub > 0 {
		return CheckResult{Name: "huggingface", Status: "skipped",
			Detail: fmt.Sprintf("huggingface_hub not installed in env — %d referenced repo(s) not verified", len(refs))}
	}
	if len(uncached) > 0 {
		cmds := make([]string, 0, len(uncached))
		ids := make([]string, 0, len(uncached))
		for _, r := range uncached {
			cmds = append(cmds, r.DownloadCommand())
			ids = append(ids, r.RepoID)
		}
		return CheckResult{
			Name:     "huggingface",
			Status:   "warning",
			Detail:   fmt.Sprintf("not in local HF cache: %s — the compute node will download at run time (fails on air-gapped clusters); pre-download to pin the exact revision", strings.Join(ids, ", ")),
			Commands: cmds,
		}
	}
	if len(res.HF) < len(refs) {
		detail := fmt.Sprintf("checked %d/%d repo(s)", len(res.HF), len(refs))
		if timedOut {
			detail = fmt.Sprintf("probe timed out after %s — %s", timeout, detail)
		}
		return CheckResult{Name: "huggingface", Status: "skipped", Detail: detail}
	}
	if unknown > 0 {
		return CheckResult{Name: "huggingface", Status: "skipped",
			Detail: fmt.Sprintf("Hub unreachable from here for %d repo(s) — nothing verified", unknown)}
	}
	scoped := fmt.Sprintf("%d repo(s) in local HF cache", cached)
	if p.Scope != "" {
		scoped += " (verified " + p.Scope + ")"
	}
	return CheckResult{Name: "huggingface", Status: "passed", Detail: scoped}
}

// checkGPU verifies GPU visibility when the project requests GPUs. The
// prerequisite (nvidia-smi on PATH) is probed first — absence is an honest
// "skipped", never a failure: on an HPC login node GPUs live elsewhere.
func checkGPU(ctx context.Context, proj *project.Config, scope string) CheckResult {
	if proj.Defaults.GPUsPerTask <= 0 {
		return CheckResult{Name: "gpu", Status: "skipped", Detail: "no GPUs requested (gpus_per_task: 0)"}
	}
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return CheckResult{Name: "gpu", Status: "skipped", Detail: "nvidia-smi not found on this node"}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, smi, "-L").Output()
	if err != nil {
		return CheckResult{Name: "gpu", Status: "failed", Detail: fmt.Sprintf("nvidia-smi -L: %v", err)}
	}
	n := strings.Count(strings.TrimSpace(string(out)), "GPU ")
	detail := fmt.Sprintf("%d GPU(s) visible", n)
	if scope != "" {
		detail += " (verified " + scope + ")"
	}
	if n < proj.Defaults.GPUsPerTask {
		detail += fmt.Sprintf(" — fewer than gpus_per_task=%d", proj.Defaults.GPUsPerTask)
	}
	return CheckResult{Name: "gpu", Status: "passed", Detail: detail}
}

// ---- (1) writability ----------------------------------------------

// checkWritable verifies that “dir“ exists and is writable by the
// current user (the runqd process). Two failure modes are split into
// distinct findings so the operator sees the actionable bit:
//
//   - doesn't exist → "create it"
//   - exists but read-only → "chmod / chown it"
//
// We probe writability with a real file write because access(W_OK)-style
// checks are platform-specific and unreliable on FUSE / NFS where ACLs lie.
// The probe runs on the check filesystem: local CreateTemp, or a remote
// WriteFile + rm through rfs.FS.
func (p Preflight) checkWritable(dir, label string) []PreflightFinding {
	if dir == "" {
		return []PreflightFinding{{Kind: "writable", Detail: fmt.Sprintf("%s is empty", label)}}
	}
	info, err := p.fsys().Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []PreflightFinding{{
				Kind:   "writable",
				Detail: fmt.Sprintf("%s does not exist: %s", label, dir),
			}}
		}
		return []PreflightFinding{{
			Kind:   "writable",
			Detail: fmt.Sprintf("%s stat failed: %v", label, err),
		}}
	}
	if !info.IsDir() {
		return []PreflightFinding{{
			Kind:   "writable",
			Detail: fmt.Sprintf("%s is not a directory: %s", label, dir),
		}}
	}
	if p.FS == nil {
		tmp, err := os.CreateTemp(dir, ".runq-preflight-*")
		if err != nil {
			return []PreflightFinding{{
				Kind:   "writable",
				Detail: fmt.Sprintf("%s not writable: %s (%v)", label, dir, err),
			}}
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil
	}
	probe := path.Join(dir, fmt.Sprintf(".runq-preflight-%d", time.Now().UnixNano()))
	if err := p.FS.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return []PreflightFinding{{
			Kind:   "writable",
			Detail: fmt.Sprintf("%s not writable: %s (%v)", label, dir, err),
		}}
	}
	_, _, _, _ = p.FS.Exec(context.Background(), "rm", "-f", probe)
	return nil
}

// ---- (2) path args -------------------------------------------------

// pathArgRegex finds shell tokens that look like file/dir paths
// inside the rendered command. Patterns it picks up:
//
//   - `--flag /abs/path` and `--flag=/abs/path`
//   - bare absolute paths (`/data/imagenet`)
//
// Limitations (documented, not bugs):
//
//   - Relative paths (`./configs/foo.yaml`) are ignored — could be
//     evaluated under working_dir but causes false positives when
//     scripts construct paths at runtime.
//   - Quoted paths containing spaces are not split correctly; tokens
//     are space-separated. Real-world job configs almost never put
//     spaces in paths, so this is acceptable.
//
// The leading boundary group is the load-bearing part: an absolute path
// must START a shell token (begin of string, whitespace, `=`, or a quote).
// Without it, relative paths (`scripts/qsub/x.sh` → "/qsub/x.sh") and
// HuggingFace model ids (`org/model` → "/model") get mangled into bogus
// absolute paths — exactly the false positives seen on real HPC commands.
var pathArgRegex = regexp.MustCompile(`(?:^|[\s='"])(/[\w./_-]+)`)

// pathArgIgnore filters out matches that should NOT be treated as
// file existence assertions. Anything not on this list and matched by
// pathArgRegex gets a stat() call.
var pathArgIgnore = map[string]bool{
	"/dev/null":   true,
	"/dev/stdout": true,
	"/dev/stderr": true,
	"/tmp":        true,
}

// checkPathArgs scans the rendered command for absolute-path tokens
// and refuses submission if any of them point at non-existent files —
// statted on the TARGET's filesystem, where the task will actually look.
// "Looks like a path" is heuristic; pathArgRegex documents the scope.
func (p Preflight) checkPathArgs(cmd string) []PreflightFinding {
	var findings []PreflightFinding
	fsys := p.fsys()
	seen := map[string]bool{}
	for _, match := range pathArgRegex.FindAllStringSubmatch(cmd, -1) {
		tok := match[1]
		if seen[tok] || pathArgIgnore[tok] {
			continue
		}
		seen[tok] = true
		// Trim trailing punctuation the regex might have grabbed.
		tok = strings.TrimRight(tok, ".,;:")
		if _, err := fsys.Stat(tok); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				findings = append(findings, PreflightFinding{
					Kind:   "path",
					Detail: fmt.Sprintf("path does not exist: %s", tok),
				})
			}
			// Stat errors other than not-exist are ambiguous (permissions,
			// transport) — no fact learned, never a finding.
		}
	}
	return findings
}

// ---- (3) imports ---------------------------------------------------

// scriptRegex extracts the python script path from a command of the
// form `python <flags> path/to/script.py ...` or `python3 script.py`.
// First .py token wins; flags ending in `=` (e.g. `-c=...`) are skipped.
var scriptRegex = regexp.MustCompile(`(?:python\d*\s+(?:-\w+\s+)*)([^\s]+\.py)\b`)

// topLevelImportRegex catches lines that match the Python import
// grammar at module top level. Handles:
//
//	import a
//	import a.b
//	import a, b, c
//	from a import x
//	from a.b import x as y
//
// Multi-line imports (`from x import (\n  y,\n  z\n)`) are partially
// supported — only the module name on the first line is captured,
// which is what we care about for the resolution check.
var topLevelImportRegex = regexp.MustCompile(
	`(?m)^[ \t]*(?:from[ \t]+([\w.]+)[ \t]+import\b|import[ \t]+([^\n#]+))`,
)

// readScript resolves and reads the python script referenced by
// `sampleCmd` (resolved against `workingDir` if relative) through the
// check filesystem. Returns ("", nil, nil) when the command references
// no python script.
func (p Preflight) readScript(sampleCmd, workingDir string) (string, []byte, error) {
	m := scriptRegex.FindStringSubmatch(sampleCmd)
	if len(m) < 2 {
		return "", nil, nil
	}
	scriptPath := m[1]
	// POSIX join: task paths are POSIX on every target runq supports (and
	// on a Windows client the remote path must never see a backslash).
	if !strings.HasPrefix(scriptPath, "/") {
		scriptPath = path.Join(workingDir, scriptPath)
	}
	src, err := p.fsys().ReadFile(scriptPath)
	if err != nil {
		return scriptPath, nil, err
	}
	return scriptPath, src, nil
}

// extractImports reads the python script referenced by `sampleCmd` and
// returns its absolute path, the top-level module names it imports, and
// any read error. Kept as the composed read+parse entry point (tests
// exercise it directly).
//
// "Top-level" means lines that start at column 0 (or after whitespace
// at the file's outer scope). Imports inside functions / classes /
// `if TYPE_CHECKING:` are intentionally not checked — they may be
// conditional and we'd produce false-positive failures.
func (p Preflight) extractImports(sampleCmd, workingDir string) (string, []string, error) {
	scriptPath, src, err := p.readScript(sampleCmd, workingDir)
	if scriptPath == "" || err != nil {
		return scriptPath, nil, err
	}
	return scriptPath, parseImports(src), nil
}

// parseImports extracts top-level module heads from python source.
func parseImports(src []byte) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range topLevelImportRegex.FindAllStringSubmatch(string(src), -1) {
		if m[1] != "" {
			// `from X import ...`
			head := topLevelModule(m[1])
			if !seen[head] {
				seen[head] = true
				names = append(names, head)
			}
			continue
		}
		// `import X, Y, Z`
		for _, raw := range strings.Split(m[2], ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			// Strip `as Y` alias.
			if i := strings.Index(raw, " as "); i >= 0 {
				raw = raw[:i]
			}
			head := topLevelModule(raw)
			if !seen[head] {
				seen[head] = true
				names = append(names, head)
			}
		}
	}
	return names
}

// topLevelModule returns the first dot-separated component of a
// dotted module path (`torch.nn.functional` → `torch`). This is what
// `import X` will resolve.
func topLevelModule(name string) string {
	if i := strings.Index(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

// pythonInterpreterRegex captures the python executable token in a command
// (`python`, `python3`, `python3.11`).
var pythonInterpreterRegex = regexp.MustCompile(`\b(python[\d.]*)\b`)

// pythonInterpreter returns the interpreter the sample command actually
// invokes, falling back to "python" when the command doesn't mention one.
func pythonInterpreter(sampleCmd string) string {
	if m := pythonInterpreterRegex.FindStringSubmatch(sampleCmd); len(m) > 1 {
		return m[1]
	}
	return "python"
}

// lastLine returns the last non-empty line of a multi-line string,
// which for shell/activation failures is the actual error (e.g.
// `sh: conda: command not found`). Avoids dumping a 20-line dump into
// the user's submit error.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// ---- helpers for SubmitJob hookup ---------------------------------

// renderSampleCommand picks the first task's rendered command from a
// freshly-expanded sweep. Preflight checks (path-arg, imports) only
// need one sample because all tasks in a sweep share the same script
// + same env; only the arg values differ.
func renderSampleCommand(proj *project.Config, taskParams []job.TaskParams, exclude map[string]bool) (string, error) {
	if len(taskParams) == 0 {
		return "", nil
	}
	// Flag-style params render with store_true semantics here too — the
	// sample must be the SAME command the plan will submit.
	var flags map[string]bool
	for _, def := range proj.Params {
		if def.Style == "flag" {
			if flags == nil {
				flags = map[string]bool{}
			}
			flags[def.Name] = true
		}
	}
	return job.RenderWithFlags(proj.CmdTemplate, taskParams[0], exclude, flags)
}
