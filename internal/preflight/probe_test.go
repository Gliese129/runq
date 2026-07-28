package preflight

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/gliese129/runq/internal/job"
	"github.com/gliese129/runq/internal/project"
)

// ----------------------------------------------------------------------
// HF reference extraction
// ----------------------------------------------------------------------

func TestExtractHFRefs(t *testing.T) {
	src := []byte(`
from transformers import AutoModel, AutoTokenizer
from datasets import load_dataset
from huggingface_hub import snapshot_download, hf_hub_download

model = AutoModel.from_pretrained("org/model-7B")
tok = AutoTokenizer.from_pretrained('org/model-7B')          # dup → dedup
base = AutoModel.from_pretrained("gpt2")                     # single-segment id
ds = load_dataset("org/eval-set", split="test")
ds2 = load_dataset(path='squad')
snap = snapshot_download(repo_id="org/weights")
one = hf_hub_download("org/cfg", filename="config.json")

local = AutoModel.from_pretrained("/data/ckpts/llama")       # abs path → skip
rel = AutoModel.from_pretrained("./out")                     # rel path → skip
home = AutoModel.from_pretrained("~/models/x")               # home path → skip
tpl = AutoModel.from_pretrained(f"org/{name}")               # f-string → skip
var = AutoModel.from_pretrained(model_name)                  # variable → skip
`)
	got := ExtractHFRefs(src)
	want := []HFRef{
		{RepoID: "org/model-7B", RepoType: "model"},
		{RepoID: "gpt2", RepoType: "model"},
		{RepoID: "org/weights", RepoType: "model"},
		{RepoID: "org/cfg", RepoType: "model"},
		{RepoID: "org/eval-set", RepoType: "dataset"},
		{RepoID: "squad", RepoType: "dataset"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %#v\nwant %#v", got, want)
	}
}

func TestHFDownloadCommand(t *testing.T) {
	if got := (HFRef{RepoID: "org/m", RepoType: "model"}).DownloadCommand(); got != "huggingface-cli download org/m" {
		t.Fatalf("model cmd = %q", got)
	}
	if got := (HFRef{RepoID: "org/d", RepoType: "dataset"}).DownloadCommand(); got != "huggingface-cli download --repo-type dataset org/d" {
		t.Fatalf("dataset cmd = %q", got)
	}
}

// ----------------------------------------------------------------------
// Probe output parsing
// ----------------------------------------------------------------------

func TestParseProbeOutput(t *testing.T) {
	out := strings.Join([]string{
		"conda activation banner noise",
		"##RUNQ_PF##\tenv\thome\tinfo\t/home/alice",
		"##RUNQ_PF##\tenv\tprefix\tinfo\t/opt/conda/envs/ml",
		"##RUNQ_PF##\timport\ttorch\tok\t",
		"##RUNQ_PF##\timport\tnope\tfail\tModuleNotFoundError: No module named 'nope'",
		"##RUNQ_PF##\thf\tmodel:org/m\treachable\t",
		"##RUNQ_PF##\tdone\tdone\tok\t",
	}, "\n")
	res := parseProbeOutput(out)
	if !res.PythonRan || !res.Done {
		t.Fatalf("ran/done = %v/%v", res.PythonRan, res.Done)
	}
	if res.Home != "/home/alice" || res.Prefix != "/opt/conda/envs/ml" {
		t.Fatalf("env facts: %q %q", res.Home, res.Prefix)
	}
	if len(res.Imports) != 2 || res.Imports[0].Status != "ok" || res.Imports[1].Status != "fail" {
		t.Fatalf("imports: %+v", res.Imports)
	}
	if !strings.Contains(res.Imports[1].Detail, "ModuleNotFoundError") {
		t.Fatalf("failure detail lost: %+v", res.Imports[1])
	}
	if len(res.HF) != 1 || res.HF[0].Status != "reachable" {
		t.Fatalf("hf: %+v", res.HF)
	}
}

func TestParseProbeOutputEmpty(t *testing.T) {
	res := parseProbeOutput("sh: conda: command not found\n")
	if res.PythonRan || res.Done {
		t.Fatalf("noise must not count as a probe run: %+v", res)
	}
}

// ----------------------------------------------------------------------
// End-to-end probe against the real local python
// ----------------------------------------------------------------------

func probePython(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"python3", "python"} {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	t.Skip("no python on PATH")
	return ""
}

func TestProbeEndToEndLocal(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	script := "import json\nimport definitely_not_a_module_zz\n"
	if err := os.WriteFile(dir+"/train.py", []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " train.py {{args}}"}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, interp+" train.py", &report)

	statuses := map[string]CheckResult{}
	for _, c := range results {
		statuses[c.Name] = c
	}
	if statuses["python_env"].Status != "passed" {
		t.Errorf("python_env: %+v", statuses["python_env"])
	}
	imp := statuses["imports"]
	if imp.Status != "failed" || !strings.Contains(imp.Detail, "definitely_not_a_module_zz") {
		t.Errorf("imports: %+v", imp)
	}
	if strings.Contains(imp.Detail, `"json"`) && strings.Contains(imp.Detail, "cannot import \"json\"") {
		t.Errorf("stdlib import wrongly failed: %+v", imp)
	}
	if report.PythonPrefix == "" || report.HomeDir == "" {
		t.Errorf("env facts not captured: %+v", report)
	}

	// The probe file must not survive the run.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".runq_preflight_") {
			t.Errorf("probe file left behind: %s", e.Name())
		}
	}
}

// Activation failure (broken env wrapper) must FAIL python_env with the
// shell's own error — this is the "conda only in ~/.bashrc" class caught
// at submit time — and skip the downstream checks.
func TestProbeActivationFailure(t *testing.T) {
	probePython(t)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/train.py", []byte("import json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "python train.py"}
	proj.PythonEnv.Type = "conda"
	proj.PythonEnv.Name = "whatever"

	// Ensure `conda` cannot be found, as on a compute node whose shell
	// never sourced ~/.bashrc.
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := exec.LookPath("conda"); err == nil {
		t.Skip("conda on trimmed PATH; cannot simulate missing conda")
	}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, "python train.py", &report)
	statuses := map[string]CheckResult{}
	for _, c := range results {
		statuses[c.Name] = c
	}
	env := statuses["python_env"]
	if env.Status != "failed" || !strings.Contains(env.Detail, "conda") {
		t.Fatalf("python_env: %+v", env)
	}
	if statuses["imports"].Status != "skipped" {
		t.Fatalf("imports after activation failure: %+v", statuses["imports"])
	}
}

// ----------------------------------------------------------------------
// Folding rules (pure)
// ----------------------------------------------------------------------

func TestImportsCheckCutShort(t *testing.T) {
	pf := DefaultPreflight()
	// Walk summary never arrived (probe cut short) -> skipped, not passed.
	res := probeOutcome{PythonRan: true, Imports: []probeImport{{Module: "a", Status: "ok"}}}
	r := pf.importsCheck(res, true, pf.ProbeTimeout)
	if r.Status != "skipped" || !strings.Contains(r.Detail, "cut short") {
		t.Fatalf("cut-short probe must skip, not pass: %+v", r)
	}
	// But a failure that DID arrive still blocks, cut short or not.
	res.Imports = append(res.Imports, probeImport{Module: "b", Status: "fail", Detail: "boom"})
	r = pf.importsCheck(res, true, pf.ProbeTimeout)
	if r.Status != "failed" {
		t.Fatalf("arrived failure must block: %+v", r)
	}
	// Self-reported truncation without failures -> skipped (unverified != verified).
	res.Imports = res.Imports[:1]
	res.ScanSeen, res.ScanTruncated, res.ScanFiles = true, true, 3
	r = pf.importsCheck(res, false, pf.ProbeTimeout)
	if r.Status != "skipped" || !strings.Contains(r.Detail, "truncated") {
		t.Fatalf("truncated walk must skip: %+v", r)
	}
}

func TestHFCheckVerdicts(t *testing.T) {
	pf := DefaultPreflight()
	m := HFRef{RepoID: "org/m", RepoType: "model"}
	d := HFRef{RepoID: "org/d", RepoType: "dataset"}

	// missing -> failed
	res := probeOutcome{PythonRan: true, Done: true, HFTotal: 2,
		HF: []probeHF{{Ref: m, Status: "missing", Detail: "404"}, {Ref: d, Status: "cached"}}}
	if r := pf.hfCheck(res, false, 0); r.Status != "failed" || !strings.Contains(r.Detail, "org/m") {
		t.Fatalf("missing: %+v", r)
	}

	// gated -> failed with token hint
	res.HF[0] = probeHF{Ref: m, Status: "gated", Detail: "401"}
	if r := pf.hfCheck(res, false, 0); r.Status != "failed" || !strings.Contains(r.Detail, "token") {
		t.Fatalf("gated: %+v", r)
	}

	// reachable-not-cached -> warning + download commands
	res.HF[0] = probeHF{Ref: m, Status: "reachable"}
	r := pf.hfCheck(res, false, 0)
	if r.Status != "warning" {
		t.Fatalf("uncached: %+v", r)
	}
	if len(r.Commands) != 1 || r.Commands[0] != "huggingface-cli download org/m" {
		t.Fatalf("commands: %+v", r.Commands)
	}

	// all cached -> passed
	res.HF[0] = probeHF{Ref: m, Status: "cached"}
	if r := pf.hfCheck(res, false, 0); r.Status != "passed" {
		t.Fatalf("cached: %+v", r)
	}

	// cut short (fewer verdicts than announced) -> skipped
	res.HF = res.HF[:1]
	if r := pf.hfCheck(res, true, 0); r.Status != "skipped" {
		t.Fatalf("cut short: %+v", r)
	}

	// no hub -> skipped (cannot verify != broken)
	res.HF = []probeHF{{Ref: m, Status: "nohub"}, {Ref: d, Status: "nohub"}}
	if r := pf.hfCheck(res, false, 0); r.Status != "skipped" {
		t.Fatalf("nohub: %+v", r)
	}

	// network unknown -> skipped
	res.HF = []probeHF{{Ref: m, Status: "unknown"}, {Ref: d, Status: "unknown"}}
	if r := pf.hfCheck(res, false, 0); r.Status != "skipped" {
		t.Fatalf("unknown: %+v", r)
	}
}

// The generated probe must be syntactically valid python even with no
// modules and no refs.
func TestProbeScriptCompiles(t *testing.T) {
	interp := probePython(t)
	for _, tc := range []struct {
		mode  string
		entry string
		refs  []HFRef
	}{
		{"", "", nil},
		{"script", "/wd/train.py", nil},
		{"module", "pkg.mod", []HFRef{{RepoID: "org/m", RepoType: "any"}}},
	} {
		script := buildProbeScript(tc.mode, tc.entry, ".", tc.refs, false, 5, 64)
		dir := t.TempDir()
		path := dir + "/probe.py"
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(interp, "-m", "py_compile", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("probe does not compile (%v): %s\n%s", err, out, script)
		}
	}
}

// ----------------------------------------------------------------------
// Contract layer (RQ-76 ②): declared = verified, undeclared = best effort
// ----------------------------------------------------------------------

func TestExpandContractHF(t *testing.T) {
	params := []job.TaskParams{
		{"model_name": "org/a", "seed": 1},
		{"model_name": "org/b", "seed": 2},
		{"model_name": "org/a", "seed": 3}, // dup value → dedup
	}
	refs, findings := expandContractHF([]string{"org/static", "{{param.model_name}}"}, params)
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %+v", findings)
	}
	want := []string{"org/static", "org/a", "org/b"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %+v, want ids %v", refs, want)
	}
	for i, r := range refs {
		if r.RepoID != want[i] || r.RepoType != "any" {
			t.Fatalf("ref %d = %+v, want id %q type any", i, r, want[i])
		}
	}

	// Unknown param and malformed id are HARD findings — a contract that
	// cannot be verified must not pass silently.
	_, findings = expandContractHF([]string{"{{param.nope}}"}, params)
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, "nope") {
		t.Fatalf("unknown param: %+v", findings)
	}
	_, findings = expandContractHF([]string{"/abs/path"}, params)
	if len(findings) != 1 {
		t.Fatalf("malformed id: %+v", findings)
	}
}

func TestShellEntry(t *testing.T) {
	if got := shellEntry("bash scripts/run.sh --x 1", "/wd"); got != "/wd/scripts/run.sh" {
		t.Fatalf("bash form: %q", got)
	}
	if got := shellEntry("./run.sh", "/wd"); got != "/wd/run.sh" {
		t.Fatalf("dot-slash form: %q", got)
	}
	if got := shellEntry("sh /abs/run.sh", "/wd"); got != "/abs/run.sh" {
		t.Fatalf("abs form: %q", got)
	}
	if got := shellEntry("python train.py", "/wd"); got != "" {
		t.Fatalf("python entry must not match: %q", got)
	}
}

// extra_run is the user's own check: non-zero exit blocks, output comes
// back as the detail. It must run even for a pure-shell project with no
// python entry at all.
func TestExtraRunContract(t *testing.T) {
	probePython(t)
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo hi"}
	proj.Preflight = &project.PreflightConfig{ExtraRun: "echo custom-broke; false"}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, "echo hi", &report)
	statuses := map[string]CheckResult{}
	for _, c := range results {
		statuses[c.Name] = c
	}
	er := statuses["extra_run"]
	if er.Status != "failed" || !strings.Contains(er.Detail, "custom-broke") {
		t.Fatalf("extra_run: %+v", er)
	}

	// Passing contract → passed.
	proj.Preflight.ExtraRun = "true"
	results = pf.probeChecks(context.Background(), proj, "echo hi", &report)
	for _, c := range results {
		if c.Name == "extra_run" && c.Status != "passed" {
			t.Fatalf("passing extra_run: %+v", c)
		}
	}
}

// Shell entry with a syntax error: bash -n catches it before anything
// else, even though there is no python to probe.
func TestShellSyntaxCheck(t *testing.T) {
	probePython(t)
	dir := t.TempDir()
	bad := "if [ 1 ]; then\necho unclosed\n" // missing fi
	if err := os.WriteFile(dir+"/run.sh", []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "bash run.sh"}

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, "bash run.sh", &report)
	statuses := map[string]CheckResult{}
	for _, c := range results {
		statuses[c.Name] = c
	}
	ss := statuses["shell_syntax"]
	if ss.Status != "failed" {
		t.Fatalf("shell_syntax: %+v", ss)
	}
	// And the imports skip explains WHERE to declare checks instead.
	if imp := statuses["imports"]; imp.Status != "skipped" || !strings.Contains(imp.Detail, "preflight.hf") {
		t.Fatalf("imports guidance: %+v", imp)
	}

	// Valid script → passed.
	if err := os.WriteFile(dir+"/run.sh", []byte("echo ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	results = pf.probeChecks(context.Background(), proj, "bash run.sh", &report)
	for _, c := range results {
		if c.Name == "shell_syntax" && c.Status != "passed" {
			t.Fatalf("valid script: %+v", c)
		}
	}
}

// python_env is decoupled from the command shape: a declared conda env is
// probed even when the entry is a shell script (the user's actual ask —
// "conda 对不对" must not depend on how the job is launched).
func TestEnvProbeDecoupledFromScript(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "bash run.sh"}
	if err := os.WriteFile(dir+"/run.sh", []byte("echo ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "venv" pointing at the real python's prefix: activation will fail
	// (no activate script) → python_env must FAIL despite shell entry.
	proj.PythonEnv.Type = "venv"
	proj.PythonEnv.Path = dir + "/no-such-venv"
	_ = interp

	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, "bash run.sh", &report)
	statuses := map[string]CheckResult{}
	for _, c := range results {
		statuses[c.Name] = c
	}
	if statuses["python_env"].Status != "failed" {
		t.Fatalf("python_env with broken venv + shell entry: %+v", statuses["python_env"])
	}
}

// Project-level master switch: preflight.enabled=false skips everything
// with one honest entry; per-run --no-preflight keeps its own wording.
func TestProjectEnabledSwitch(t *testing.T) {
	dir := t.TempDir()
	off := false
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: "echo hi",
		Preflight: &project.PreflightConfig{Enabled: &off}}
	rep := DefaultPreflight().RunPreflight(context.Background(), proj, "echo hi")
	if len(rep.Results) != 1 || rep.Results[0].Status != "skipped" ||
		!strings.Contains(rep.Results[0].Detail, "preflight.enabled") {
		t.Fatalf("enabled=false: %+v", rep.Results)
	}
}

// imports:false gates the static tier without touching the rest.
func TestImportsSwitch(t *testing.T) {
	interp := probePython(t)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/train.py", []byte("import definitely_not_a_module_zz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off := false
	proj := &project.Config{ProjectName: "p", WorkingDir: dir, CmdTemplate: interp + " train.py",
		Preflight: &project.PreflightConfig{Imports: &off}}
	pf := DefaultPreflight()
	var report Report
	results := pf.probeChecks(context.Background(), proj, interp+" train.py", &report)
	for _, c := range results {
		if c.Name == "imports" && c.Status != "skipped" {
			t.Fatalf("imports with switch off: %+v", c)
		}
	}
}
