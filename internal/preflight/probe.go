package preflight

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

// The single-probe design (RQ-76 ②): ONE generated python file, ONE
// shell, ONE env activation. Since Codex r3 the probe also owns the
// whole dependency analysis — see probe.py.tmpl for the rationale
// (ast + CPython's own resolver instead of Go-side regexes) — which
// has a second structural benefit: discovery reads files LOCALLY on
// the target inside the probe process, so it sits under the same
// process-group wall-clock kill as everything else. No more serial
// remote Stat/ReadFile outside the timeout.
//
// The probe prints machine-readable marker lines; Go parses them back
// into the four-state CheckResult grammar. Ordering inside the probe is
// deliberate — env facts → import walk → wandb → HF — so a timeout
// still yields partial results (whatever markers arrived are used; the
// rest report as skipped, never failed: a timeout learns no fact).
//
// The probe runs in a NON-LOGIN, NON-INTERACTIVE shell (`sh -c` over
// SSH) — the same shell class compute nodes give the task. That is a
// feature: a conda that is only registered in ~/.bashrc fails HERE, at
// submit time, instead of after two hours in the queue (RQ-76 ①).

// probeMarker prefixes every machine-readable line the probe emits.
// Fields are tab-separated: MARKER \t kind \t key \t status \t detail.
const probeMarker = "##RUNQ_PF##"

//go:embed probe.py.tmpl
var probeTemplate string

const wandbSection = `
netrc = os.path.join(os.path.expanduser("~"), ".netrc")
has_cred = bool(os.environ.get("WANDB_API_KEY"))
if not has_cred:
    try:
        with open(netrc) as f:
            has_cred = "api.wandb.ai" in f.read()
    except BaseException:
        pass
emit("wandb", "cred", "ok" if has_cred else "missing")
`

// pyq renders s as a python string literal. Go's %q escaping (\", \\,
// \xNN, \uNNNN) is valid python string syntax.
func pyq(s string) string {
	return fmt.Sprintf("%q", s)
}

// buildProbeScript instantiates probe.py.tmpl. entryMode is "script"
// (entry = absolute .py path), "module" (entry = the -m dotted name) or
// "" (no python entry: contract-only probe). budget bounds the
// dependency walk INSIDE the probe (seconds); maxFiles caps scanned
// project files. Contract refs carry repo_type "any" (model first,
// then dataset).
func buildProbeScript(entryMode, entry, workdir string, contract []HFRef, wandb bool, budget float64, maxFiles int) string {
	var refs strings.Builder
	refs.WriteString("[")
	for _, r := range contract {
		fmt.Fprintf(&refs, "(%s, %s), ", pyq(r.RepoID), pyq(r.RepoType))
	}
	refs.WriteString("]")
	ws := ""
	if wandb {
		ws = wandbSection
	}
	return strings.NewReplacer(
		"__MARK__", probeMarker,
		"__WORKDIR__", pyq(workdir),
		"__ENTRY_MODE__", pyq(entryMode),
		"__ENTRY__", pyq(entry),
		"__BUDGET__", fmt.Sprintf("%.1f", budget),
		"__MAXFILES__", strconv.Itoa(maxFiles),
		"__CONTRACT_REFS__", refs.String(),
		"__WANDB_SECTION__", ws,
	).Replace(probeTemplate)
}

// probeImport is one module's verdict: ok | fail. Chain misses on
// import statements are CERTAIN failures for locals and externals
// alike — `import a.b` imports the module a.b, and PEP 562 __getattr__
// rescues attribute access, never import statements (Codex r4 ruling).
type probeImport struct {
	Module string
	Status string
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

	// Scan facts self-reported by the probe's dependency walk.
	ScanSeen      bool // the walk finished (its summary marker arrived)
	ScanFiles     int
	ScanTruncated bool

	// EntryMiss: the entry could not be located from working_dir (detail
	// = the missed path/module). DOCTRINE, not detection (user ruling
	// r4): paths resolve from working_dir, runq does not follow `cd` —
	// folded to skipped + guidance, NEVER a failure.
	EntryMiss string

	// HFTotal: how many refs the probe intended to check (discovered +
	// contract) — lets Go detect a cut-short HF pass without knowing the
	// discovered set in advance.
	HFTotal int

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
func parseProbeOutput(out string) probeOutcome {
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
			res.Imports = append(res.Imports, probeImport{Module: key, Status: status, Detail: detail})
		case "entrymiss":
			res.PythonRan = true
			res.EntryMiss = detail
		case "scan":
			res.PythonRan = true
			res.ScanSeen = true
			if n, err := strconv.Atoi(status); err == nil {
				res.ScanFiles = n
			}
			res.ScanTruncated = detail == "truncated"
		case "wandb":
			res.PythonRan = true
			res.Wandb = status
		case "hftotal":
			res.PythonRan = true
			if n, err := strconv.Atoi(status); err == nil {
				res.HFTotal = n
			}
		case "hf":
			res.PythonRan = true
			rt, rid, ok := strings.Cut(key, ":")
			if !ok {
				continue
			}
			res.HF = append(res.HF, probeHF{Ref: HFRef{RepoID: rid, RepoType: rt}, Status: status, Detail: detail})
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
