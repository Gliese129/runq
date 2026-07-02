package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
	"github.com/gliese129/runq/internal/rfs"
	"github.com/gliese129/runq/internal/utils"
)

// Preflight is the L2-C step 10 fail-fast check that runs **before** a
// job is persisted + queued. The doc (F8 in stage2_sdk_design.md) lists
// four classes of failure that should be caught at submit time, not
// after the task waits 2 hours in a queue:
//
//  1. **Imports**: the target script imports a module that the project
//     Python env cannot resolve.
//  2. **pip check**: the project env has resolved-but-incompatible
//     dependency versions.
//  3. **Path args**: the rendered command references an absolute path
//     that does not exist on disk.
//  4. **Writability**: working_dir or task_dir parent is not writable.
//
// Each check returns a `PreflightFinding` describing the problem; the
// final `PreflightReport` aggregates them. Empty findings → submission
// proceeds. Non-empty → submission is refused with a single combined
// error message.
//
// The `Skip` flag lets the CLI bypass the whole thing with
// `runq submit --no-preflight`; users with conditional imports or
// runtime path inference can opt out.
type Preflight struct {
	// Skip disables every check and makes RunPreflight a no-op. Wired
	// to the CLI's --no-preflight flag.
	Skip bool

	// PipCheckTimeout caps the `python -m pip check` subprocess. pip
	// resolution can pause on slow disks; bound it so submission
	// never hangs indefinitely. Default 10s.
	PipCheckTimeout time.Duration

	// ImportTimeout caps the per-module `python -c "import X"` probe.
	// Default 5s — most imports finish in <100ms, but heavy native
	// modules (torch, jax) can take 1-2s on cold cache.
	ImportTimeout time.Duration

	// DisableLocal turns the local-subprocess checks (imports, pip) into
	// skipped results — for clusters whose login-node policy forbids them
	// (hpc: preflight_local: false).
	DisableLocal bool

	// Scope labels where local checks ran (e.g. "on login node") so HPC
	// users see the verification boundary in passed results.
	Scope string

	// ExcludeParams are scheduler-consumed params (submit_template's
	// {{param.*}}) — the sample command renders without demanding them.
	ExcludeParams map[string]bool

	// FS is the filesystem THE TASK will run against. nil = local os
	// semantics (current machine). For remote targets this is the target's
	// rfs.FS: path/writability checks stat the REMOTE filesystem, the
	// script is read from it, and interpreter probes execute on the remote
	// login node — checking the client machine would answer a question
	// nobody asked. GPU checks are skipped for remote (login nodes have no
	// GPUs; the probe would only produce noise).
	FS rfs.FS
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
// a finding. A non-zero exit code is the probe's own verdict.
func (p Preflight) runShell(ctx context.Context, cmd string) (string, int, error) {
	if p.FS == nil {
		out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
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

// DefaultPreflight returns a preflight runner with sane defaults.
func DefaultPreflight() Preflight {
	return Preflight{
		PipCheckTimeout: 10 * time.Second,
		ImportTimeout:   5 * time.Second,
	}
}

// Run executes preflight against a project and expanded task params,
// rendering the sample command internally ("first task is representative").
// Returns the three-state report; report.Err() is the blocking error.
func (p Preflight) Run(ctx context.Context, proj *project.Config, taskParams []job.TaskParams) (Report, error) {
	sampleCmd, err := renderSampleCommand(proj, taskParams, p.ExcludeParams)
	if err != nil {
		return Report{}, err
	}
	return p.RunPreflight(ctx, proj, sampleCmd), nil
}

// PreflightFinding is one failed check.
type PreflightFinding struct {
	Kind   string // "import" | "pip_check" | "path" | "writable"
	Detail string
}

func (f PreflightFinding) String() string {
	return fmt.Sprintf("  - %s: %s", f.Kind, f.Detail)
}

// CheckResult is one check in the three-state grammar shared with
// hpcconfig.Check: passed / failed / skipped. skipped means "could not be
// checked HERE" (missing prerequisite, disabled by config) — it never
// blocks a submit and must never be presented as a failure.
type CheckResult struct {
	Name   string `json:"name"`   // writable | paths | imports | pip_check | gpu
	Status string `json:"status"` // passed | failed | skipped
	Detail string `json:"detail,omitempty"`
}

// Report aggregates all check results. Only failed entries block.
type Report struct {
	Results []CheckResult `json:"results"`
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

// fold collapses a category's findings into one three-state result. scope
// ("on login node") is appended to passed local checks — login-node imports
// passing does not guarantee the compute-node environment, and the report
// should say so rather than imply more than was verified.
func fold(name string, findings []PreflightFinding, scope string) CheckResult {
	if len(findings) > 0 {
		details := make([]string, 0, len(findings))
		for _, f := range findings {
			details = append(details, f.Detail)
		}
		return CheckResult{name, "failed", strings.Join(details, "; ")}
	}
	detail := "ok"
	if scope != "" {
		detail = "ok (verified " + scope + ")"
	}
	return CheckResult{name, "passed", detail}
}

// RunPreflight executes all four checks against the project config + a
// **sample rendered command** (the first task's command is the natural
// choice — all tasks in a sweep share the same script + same env, only
// the args differ, and path-arg / import checks are independent of which
// concrete arg values are used).
//
// SubmitJob is expected to:
//
//	pf := DefaultPreflight()
//	pf.Skip = noPreflightFlag
//	if err := pf.RunPreflight(ctx, proj, sampleCmd); err != nil { return err }
//
// before persisting the job.
func (p Preflight) RunPreflight(ctx context.Context, proj *project.Config, sampleCmd string) Report {
	if p.Skip {
		return Report{Results: []CheckResult{{"preflight", "skipped", "disabled by --no-preflight"}}}
	}
	var results []CheckResult

	// Tier free: filesystem checks — against the TARGET's filesystem.
	results = append(results, fold("writable", p.checkWritable(proj.WorkingDir, "working_dir"), p.Scope))
	results = append(results, fold("paths", p.checkPathArgs(sampleCmd), p.Scope))

	// Tier cheap: local subprocesses (python imports, pip). Skippable by
	// cluster policy; scope-labelled because "here" may not be the
	// execution node.
	if p.DisableLocal {
		results = append(results,
			CheckResult{"imports", "skipped", "disabled by hpc preflight_local"},
			CheckResult{"pip_check", "skipped", "disabled by hpc preflight_local"})
	} else {
		// Probe with THE interpreter the task will run — never a hardcoded
		// "python". A `python3`-only environment (most modern distros ship
		// no bare `python`) would otherwise fail every import probe and
		// train users to always pass --no-preflight.
		interp := pythonInterpreter(sampleCmd)
		scriptPath, importNames, _ := p.extractImports(sampleCmd, proj.WorkingDir)
		if scriptPath == "" {
			results = append(results, CheckResult{"imports", "skipped", "no python script in command"})
		} else {
			results = append(results, fold("imports", p.checkImports(ctx, proj, interp, importNames, p.ImportTimeout), p.Scope))
		}
		results = append(results, fold("pip_check", p.checkPipCheck(ctx, proj, interp, p.PipCheckTimeout), p.Scope))
	}

	// GPU smoke test: probe-don't-enumerate (C2). No driver here ≠ failure.
	// Remote targets skip outright: GPUs live on compute nodes, and probing
	// a login node's nvidia-smi would only produce noise.
	if p.FS != nil {
		results = append(results, CheckResult{"gpu", "skipped", "remote target: GPUs live on compute nodes"})
	} else {
		results = append(results, checkGPU(ctx, proj, p.Scope))
	}

	return Report{Results: results}
}

// checkGPU verifies GPU visibility when the project requests GPUs. The
// prerequisite (nvidia-smi on PATH) is probed first — absence is an honest
// "skipped", never a failure: on an HPC login node GPUs live elsewhere.
func checkGPU(ctx context.Context, proj *project.Config, scope string) CheckResult {
	if proj.Defaults.GPUsPerTask <= 0 {
		return CheckResult{"gpu", "skipped", "no GPUs requested (gpus_per_task: 0)"}
	}
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return CheckResult{"gpu", "skipped", "nvidia-smi not found on this node"}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, smi, "-L").Output()
	if err != nil {
		return CheckResult{"gpu", "failed", fmt.Sprintf("nvidia-smi -L: %v", err)}
	}
	n := strings.Count(strings.TrimSpace(string(out)), "GPU ")
	detail := fmt.Sprintf("%d GPU(s) visible", n)
	if scope != "" {
		detail += " (verified " + scope + ")"
	}
	if n < proj.Defaults.GPUsPerTask {
		detail += fmt.Sprintf(" — fewer than gpus_per_task=%d", proj.Defaults.GPUsPerTask)
	}
	return CheckResult{"gpu", "passed", detail}
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

// extractImports reads the python script referenced by `sampleCmd`
// (resolved against `workingDir` if relative) and returns:
//
//   - the absolute path of the script
//   - the list of top-level module names it imports
//   - any error encountered while reading the script
//
// stdlib modules and obviously-local imports (single-letter names,
// names matching the script's own basename) are NOT filtered here;
// the import check loop is responsible for ignoring stdlib hits in
// its results so we don't have to maintain a stdlib allowlist.
//
// "Top-level" means lines that start at column 0 (or after whitespace
// at the file's outer scope). Imports inside functions / classes /
// `if TYPE_CHECKING:` are intentionally not checked — they may be
// conditional and we'd produce false-positive failures.
func (p Preflight) extractImports(sampleCmd, workingDir string) (string, []string, error) {
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
		// Not finding the script is a finding in its own right; the
		// path-args check above usually catches it first, but we
		// surface a clean message either way.
		return scriptPath, nil, err
	}
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
	return scriptPath, names, nil
}

// topLevelModule returns the first dot-separated component of a
// dotted module path (`torch.nn.functional` → `torch`). This is what
// `python -c "import X"` will resolve.
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

// checkImports probes each module with `<interp> -c "import X"` inside
// the project env, ON the target (locally, or over FS.Exec on a remote
// login node). A non-zero exit → finding with the actual import error;
// a probe that could not run at all (transport) is silently no-fact.
// Skips modules that look like local modules of the project.
func (p Preflight) checkImports(ctx context.Context, proj *project.Config, interp string, modules []string, timeout time.Duration) []PreflightFinding {
	if len(modules) == 0 {
		return nil
	}
	// Build a Set of plausible local module names so we don't try to
	// resolve them via pip / site-packages. Read from the target FS:
	// the project lives where the task runs.
	local := p.localModuleNames(proj.WorkingDir)

	var findings []PreflightFinding
	for _, mod := range modules {
		if local[mod] {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := utils.WrapCommand(
			proj.PythonEnv.Type, proj.PythonEnv.Path, proj.PythonEnv.Name,
			fmt.Sprintf("%s -c %q", interp, "import "+mod),
			proj.WorkingDir,
		)
		out, code, rerr := p.runShell(cctx, cmd)
		cancel()
		if rerr != nil {
			continue // probe couldn't run — no fact learned, never a finding
		}
		if code != 0 {
			findings = append(findings, PreflightFinding{
				Kind:   "import",
				Detail: fmt.Sprintf("cannot import %q in env: %s", mod, lastLine(out)),
			})
		}
	}
	return findings
}

// localModuleNames returns the set of top-level identifiers that the
// project itself defines, so the import probe doesn't fail on
// "cannot import 'mylib'" for files that live alongside the script.
func (p Preflight) localModuleNames(workingDir string) map[string]bool {
	out := map[string]bool{}
	entries, err := p.fsys().ReadDir(workingDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()] = true
			continue
		}
		if strings.HasSuffix(e.Name(), ".py") {
			out[strings.TrimSuffix(e.Name(), ".py")] = true
		}
	}
	return out
}

// lastLine returns the last non-empty line of a multi-line string,
// which for Python tracebacks is the actual exception (e.g.
// `ModuleNotFoundError: No module named 'transformers'`). Avoids
// dumping a 20-line traceback into the user's submit error.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// ---- (4) pip check -------------------------------------------------

// checkPipCheck runs `python -m pip check` inside the project env. The
// output is either empty (env is consistent) or a series of lines
// like:
//
//	tensorflow 2.5.0 has requirement numpy~=1.19.2, but you have numpy 1.24.0.
//
// Each non-empty line becomes a finding. Subprocess timeouts and
// generic failures are NOT treated as preflight failures (we don't
// know if it's a pip-was-busy situation vs. a real env break); they're
// silently dropped so this check stays advisory.
func (p Preflight) checkPipCheck(ctx context.Context, proj *project.Config, interp string, timeout time.Duration) []PreflightFinding {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := utils.WrapCommand(
		proj.PythonEnv.Type, proj.PythonEnv.Path, proj.PythonEnv.Name,
		interp+" -m pip check",
		proj.WorkingDir,
	)
	out, _, rerr := p.runShell(cctx, cmd)
	if cctx.Err() != nil || rerr != nil {
		return nil // advisory check: timeout / can't-run is never a finding
	}
	// pip exits non-zero when it finds issues; the output content is
	// what we care about, not the exit code.
	text := strings.TrimSpace(out)
	if text == "" || text == "No broken requirements found." {
		return nil
	}
	var findings []PreflightFinding
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "No broken requirements found.") {
			continue
		}
		findings = append(findings, PreflightFinding{
			Kind:   "pip_check",
			Detail: line,
		})
	}
	return findings
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
	return job.RenderExcluding(proj.CmdTemplate, taskParams[0], exclude)
}
