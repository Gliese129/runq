package preflight

import (
	"fmt"
	"strconv"
	"strings"
)

// The single-probe design (RQ-76 ②): instead of one `conda activate &&
// python -c "import X"` round trip PER MODULE (the old shape — a dozen
// serial SSH execs, each paying 1-3s of conda activation, routinely
// blowing the dashboard's request timeout), preflight now generates ONE
// python probe file, uploads it, and executes it in ONE shell:
//
//	[bash -n entry.sh] → activate env once → import every module →
//	verify HF repos → wandb creds → extra_run contract → done
//
// The probe prints machine-readable marker lines; Go parses them back
// into the existing four-state CheckResult grammar. Ordering inside the
// probe is deliberate — cheap facts first — so a timeout still yields
// partial results (whatever markers arrived are used; the rest report
// as skipped, never failed: a timeout learns no fact).
//
// The probe runs in a NON-LOGIN, NON-INTERACTIVE shell (`sh -c` over
// SSH) — the same shell class compute nodes give the task. That is a
// feature: a conda that is only registered in ~/.bashrc fails HERE, at
// submit time, instead of after two hours in the queue (RQ-76 ①).

// probeMarker prefixes every machine-readable line the probe emits.
// Fields are tab-separated: MARKER \t kind \t key \t status \t detail.
const probeMarker = "##RUNQ_PF##"

// buildProbeScript renders the python probe source. Module names and
// repo ids were validated by the extraction regexes (`[\w.]` heads,
// hfRepoIDRegex) — %q quoting makes them safe python string literals
// regardless. Declared contract refs carry repo_type "any": the probe
// tries the model namespace first, then dataset, and only reports
// missing when both miss.
func buildProbeScript(modules []string, refs []HFRef, wandb bool) string {
	var b strings.Builder
	b.WriteString(`import importlib, os, sys

def emit(kind, key, status, detail=""):
    detail = str(detail).replace("\t", " ").replace("\n", " ")
    print("\t".join(["` + probeMarker + `", kind, key, status, detail]), flush=True)

emit("env", "home", "info", os.path.expanduser("~"))
emit("env", "prefix", "info", sys.prefix)

MODULES = [`)
	for _, m := range modules {
		fmt.Fprintf(&b, "%q, ", m)
	}
	b.WriteString(`]
for m in MODULES:
    try:
        importlib.import_module(m)
        emit("import", m, "ok")
    except BaseException as e:
        emit("import", m, "fail", "%s: %s" % (type(e).__name__, e))
`)
	if wandb {
		b.WriteString(`
netrc = os.path.join(os.path.expanduser("~"), ".netrc")
has_cred = bool(os.environ.get("WANDB_API_KEY"))
if not has_cred:
    try:
        with open(netrc) as f:
            has_cred = "api.wandb.ai" in f.read()
    except BaseException:
        pass
emit("wandb", "cred", "ok" if has_cred else "missing")
`)
	}
	b.WriteString(`
HF_REFS = [`)
	for _, r := range refs {
		fmt.Fprintf(&b, "(%q, %q), ", r.RepoID, r.RepoType)
	}
	b.WriteString(`]
if HF_REFS:
    hub = None
    try:
        import huggingface_hub as hub
    except BaseException:
        for rid, rt in HF_REFS:
            emit("hf", rt + ":" + rid, "nohub")
    if hub is not None:
        # Resolution goes through huggingface_hub itself — the SAME cache
        # lookup (HF_HOME / HF_HUB_CACHE) and the SAME token discovery the
        # task will use at run time, so the verdict is reproducible.
        cached = set()
        try:
            for repo in hub.scan_cache_dir().repos:
                cached.add(repo.repo_type + ":" + repo.repo_id)
        except BaseException:
            pass
        api = hub.HfApi()

        def probe_one(rid, rt):
            try:
                api.repo_info(rid, repo_type=rt, timeout=5)
                return "reachable", ""
            except BaseException as e:
                name = type(e).__name__
                first = (str(e).splitlines() or [""])[0]
                if name == "RepositoryNotFoundError":
                    return "missing", first or name
                if name == "GatedRepoError":
                    return "gated", first or name
                return "unknown", name

        for rid, rt in HF_REFS:
            key = rt + ":" + rid
            types = ["model", "dataset"] if rt == "any" else [rt]
            if any((t + ":" + rid) in cached for t in types):
                emit("hf", key, "cached")
                continue
            status, detail = "missing", ""
            for t in types:
                s, d = probe_one(rid, t)
                if s != "missing":
                    status, detail = s, d
                    break
                detail = d
            emit("hf", key, status, detail)

emit("done", "done", "ok")
`)
	return b.String()
}

// probeImport is one module's verdict from the probe.
type probeImport struct {
	Module string
	OK     bool
	Detail string
}

// probeHF is one Hub ref's verdict: cached | reachable | missing |
// gated | nohub | unknown.
type probeHF struct {
	Ref    HFRef
	Status string
	Detail string
}

// probeOutcome is the parsed probe run.
type probeOutcome struct {
	// PythonRan: python markers arrived — env activation AND the
	// interpreter both work. Done: the final marker arrived (nothing cut
	// off by timeout/crash). InnerRan: the wrapped chain got past env
	// activation (the shell-emitted pyexit marker arrived), even if the
	// interpreter itself was missing.
	PythonRan bool
	Done      bool
	InnerRan  bool
	// PyExit is the python interpreter's exit code as seen by the shell
	// (-1 when the marker never arrived). 127 = interpreter not found.
	PyExit int

	Home    string
	Prefix  string
	Imports []probeImport
	HF      []probeHF

	// Wandb: "" (not checked) | "ok" | "missing".
	Wandb string

	// ShellSyntax: bash -n verdict for a .sh entry. RC -1 = not run.
	ShellSyntaxRC     int
	ShellSyntaxDetail string

	// ExtraRun contract: begin/end markers bracket its combined output.
	ExtraStarted bool
	ExtraRC      int // -1 until the end marker arrives
	ExtraOut     []string
}

// parseProbeOutput extracts marker lines from combined probe output.
// Non-marker noise (activation banners, warnings libraries print on
// import) is ignored — except between the extra begin/end markers,
// where it IS the custom check's output.
func parseProbeOutput(out string, refs []HFRef) probeOutcome {
	byKey := map[string]HFRef{}
	for _, r := range refs {
		byKey[r.RepoType+":"+r.RepoID] = r
	}
	res := probeOutcome{PyExit: -1, ShellSyntaxRC: -1, ExtraRC: -1}
	inExtra := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, probeMarker+"\t") {
			if inExtra && line != "" {
				res.ExtraOut = append(res.ExtraOut, line)
			}
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 4 {
			continue
		}
		kind, key, status := parts[1], parts[2], parts[3]
		detail := ""
		if len(parts) == 5 {
			detail = parts[4]
		}
		switch kind {
		case "env":
			res.PythonRan = true
			switch key {
			case "home":
				res.Home = detail
			case "prefix":
				res.Prefix = detail
			}
		case "import":
			res.PythonRan = true
			res.Imports = append(res.Imports, probeImport{Module: key, OK: status == "ok", Detail: detail})
		case "wandb":
			res.PythonRan = true
			res.Wandb = status
		case "hf":
			res.PythonRan = true
			ref, ok := byKey[key]
			if !ok {
				continue
			}
			res.HF = append(res.HF, probeHF{Ref: ref, Status: status, Detail: detail})
		case "done":
			res.PythonRan = true
			res.Done = true
		case "pyexit":
			res.InnerRan = true
			if n, err := strconv.Atoi(status); err == nil {
				res.PyExit = n
			}
		case "shellsyntax":
			if n, err := strconv.Atoi(status); err == nil {
				res.ShellSyntaxRC = n
			}
			res.ShellSyntaxDetail = detail
		case "extra":
			switch key {
			case "begin":
				res.ExtraStarted = true
				inExtra = true
			case "end":
				inExtra = false
				if n, err := strconv.Atoi(status); err == nil {
					res.ExtraRC = n
				}
			}
		}
	}
	return res
}

// shQuote single-quotes s for POSIX shells.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
