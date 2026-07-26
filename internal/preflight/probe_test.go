package preflight

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

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
	refs := []HFRef{{RepoID: "org/m", RepoType: "model"}}
	out := strings.Join([]string{
		"conda activation banner noise",
		"##RUNQ_PF##\tenv\thome\tinfo\t/home/alice",
		"##RUNQ_PF##\tenv\tprefix\tinfo\t/opt/conda/envs/ml",
		"##RUNQ_PF##\timport\ttorch\tok\t",
		"##RUNQ_PF##\timport\tnope\tfail\tModuleNotFoundError: No module named 'nope'",
		"##RUNQ_PF##\thf\tmodel:org/m\treachable\t",
		"##RUNQ_PF##\tdone\tdone\tok\t",
	}, "\n")
	res := parseProbeOutput(out, refs)
	if !res.Ran || !res.Done {
		t.Fatalf("ran/done = %v/%v", res.Ran, res.Done)
	}
	if res.Home != "/home/alice" || res.Prefix != "/opt/conda/envs/ml" {
		t.Fatalf("env facts: %q %q", res.Home, res.Prefix)
	}
	if len(res.Imports) != 2 || res.Imports[0].OK != true || res.Imports[1].OK != false {
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
	res := parseProbeOutput("sh: conda: command not found\n", nil)
	if res.Ran || res.Done {
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
	modules := []string{"a", "b", "c"}
	res := probeOutcome{Ran: true, Imports: []probeImport{{Module: "a", OK: true}}}
	r := pf.importsCheck(modules, res, true, pf.ProbeTimeout)
	if r.Status != "skipped" || !strings.Contains(r.Detail, "1/3") {
		t.Fatalf("cut-short probe must skip, not pass: %+v", r)
	}
	// But a failure that DID arrive still blocks, cut short or not.
	res.Imports = append(res.Imports, probeImport{Module: "b", OK: false, Detail: "boom"})
	r = pf.importsCheck(modules, res, true, pf.ProbeTimeout)
	if r.Status != "failed" {
		t.Fatalf("arrived failure must block: %+v", r)
	}
}

func TestHFCheckVerdicts(t *testing.T) {
	pf := DefaultPreflight()
	m := HFRef{RepoID: "org/m", RepoType: "model"}
	d := HFRef{RepoID: "org/d", RepoType: "dataset"}
	refs := []HFRef{m, d}

	// missing → failed
	res := probeOutcome{Ran: true, Done: true, HF: []probeHF{{Ref: m, Status: "missing", Detail: "404"}, {Ref: d, Status: "cached"}}}
	if r := pf.hfCheck(refs, res, false, 0); r.Status != "failed" || !strings.Contains(r.Detail, "org/m") {
		t.Fatalf("missing: %+v", r)
	}

	// gated → failed with token hint
	res.HF[0] = probeHF{Ref: m, Status: "gated", Detail: "401"}
	if r := pf.hfCheck(refs, res, false, 0); r.Status != "failed" || !strings.Contains(r.Detail, "token") {
		t.Fatalf("gated: %+v", r)
	}

	// reachable-not-cached → warning + download commands
	res.HF[0] = probeHF{Ref: m, Status: "reachable"}
	r := pf.hfCheck(refs, res, false, 0)
	if r.Status != "warning" {
		t.Fatalf("uncached: %+v", r)
	}
	if len(r.Commands) != 1 || r.Commands[0] != "huggingface-cli download org/m" {
		t.Fatalf("commands: %+v", r.Commands)
	}

	// all cached → passed
	res.HF[0] = probeHF{Ref: m, Status: "cached"}
	if r := pf.hfCheck(refs, res, false, 0); r.Status != "passed" {
		t.Fatalf("cached: %+v", r)
	}

	// no hub → skipped (cannot verify ≠ broken)
	res.HF = []probeHF{{Ref: m, Status: "nohub"}, {Ref: d, Status: "nohub"}}
	if r := pf.hfCheck(refs, res, false, 0); r.Status != "skipped" {
		t.Fatalf("nohub: %+v", r)
	}

	// network unknown → skipped
	res.HF = []probeHF{{Ref: m, Status: "unknown", Detail: "ConnectionError"}, {Ref: d, Status: "unknown", Detail: "ConnectionError"}}
	if r := pf.hfCheck(refs, res, false, 0); r.Status != "skipped" {
		t.Fatalf("unknown: %+v", r)
	}
}

// The generated probe must be syntactically valid python even with no
// modules and no refs.
func TestProbeScriptCompiles(t *testing.T) {
	interp := probePython(t)
	for _, tc := range []struct {
		modules []string
		refs    []HFRef
	}{
		{nil, nil},
		{[]string{"json", "os"}, nil},
		{[]string{"json"}, []HFRef{{RepoID: "org/m", RepoType: "model"}}},
	} {
		script := buildProbeScript(tc.modules, tc.refs)
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
