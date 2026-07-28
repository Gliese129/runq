package preflight

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// Source discovery (Codex r1 #2 / r2 #1): import/HF analysis follows the
// code the task will actually run —
//
//   - a `bash run.sh` entry is scanned for the python invocations INSIDE
//     the script (one level: run.sh → train.py),
//   - local imports are walked as a dependency graph, and DOTTED local
//     imports resolve down the whole chain (`from pkg.helper import x`
//     scans pkg/__init__.py AND pkg/helper.py),
//
// bounded three ways: a visited set, a file cap, and the caller's probe
// deadline (each remote Stat/ReadFile is a round trip — discovery checks
// ctx between reads and truncates instead of overrunning the budget).
//
// The philosophy behind what gets checked (user-aligned): import
// failures in the wild are almost never "the package is broken" — they
// are "the env didn't activate" or "the import root / package layout is
// wrong". So the Go side statically verifies the LOCAL layout (files
// exist where the dotted path says), and the probe verifies
// RESOLVABILITY in the task's exact sys.path — not deep package health.

// maxSourceFiles caps how many python files the graph walk reads.
const maxSourceFiles = 32

// pySource is one python file pulled into the analysis.
type pySource struct {
	Path string
	Src  []byte
}

// discovery is the result of source collection + graph walk.
type discovery struct {
	// External: third-party module heads to resolve in the probe.
	External []string
	// LocalTop: top-level local module names — probed with find_spec in
	// the task's sys.path (catches import-root mismatches) without
	// executing project code.
	LocalTop []string
	// Scanned: every source read (entry + shell-invoked + local deps),
	// the HF extraction surface.
	Scanned []pySource
	// LayoutFindings: dotted local imports whose module files do not
	// exist (`from pkg.helper import x` but no pkg/helper.py) — static
	// facts, hard findings.
	LayoutFindings []PreflightFinding
	// ScriptDir is the entry script's directory — python puts it at
	// sys.path[0], and the probe replicates that.
	ScriptDir string
	// Truncated: the walk hit the deadline or the file cap.
	Truncated bool
}

// collectEntrySources resolves the entry command into its python sources:
// a .py entry directly, a .sh entry via the python invocations found in
// the script text. Returns the sources, the resolved .sh path (when the
// entry is a shell script), and the first read error for the ENTRY file
// (secondary reads are best-effort).
func (p Preflight) collectEntrySources(ctx context.Context, sampleCmd, workingDir string) (sources []pySource, shPath string, err error) {
	resolve := func(s string) string {
		if !strings.HasPrefix(s, "/") {
			return path.Join(workingDir, s)
		}
		return s
	}
	if m := scriptRegex.FindStringSubmatch(sampleCmd); len(m) >= 2 {
		sp := resolve(m[1])
		src, rerr := p.fsys().ReadFile(sp)
		if rerr != nil {
			return nil, "", rerr
		}
		return []pySource{{Path: sp, Src: src}}, "", nil
	}
	shPath = shellEntry(sampleCmd, workingDir)
	if shPath == "" {
		return nil, "", nil
	}
	shSrc, rerr := p.fsys().ReadFile(shPath)
	if rerr != nil {
		return nil, shPath, rerr
	}
	seen := map[string]bool{}
	for _, m := range scriptRegex.FindAllStringSubmatch(string(shSrc), -1) {
		if ctx.Err() != nil {
			break
		}
		sp := resolve(m[1])
		if seen[sp] {
			continue
		}
		seen[sp] = true
		src, rerr := p.fsys().ReadFile(sp)
		if rerr != nil {
			continue // referenced-but-unreadable: the paths check owns missing files
		}
		sources = append(sources, pySource{Path: sp, Src: src})
	}
	return sources, shPath, nil
}

// resolveModuleFile maps a dotted module prefix to the file that defines
// it under workingDir: a/b → a/b.py or a/b/__init__.py. Returns
// (file, isPackage, exists).
func (p Preflight) resolveModuleFile(workingDir string, parts []string) (string, bool, bool) {
	base := path.Join(append([]string{workingDir}, parts...)...)
	if _, err := p.fsys().Stat(base + ".py"); err == nil {
		return base + ".py", false, true
	}
	if _, err := p.fsys().Stat(path.Join(base, "__init__.py")); err == nil {
		return path.Join(base, "__init__.py"), true, true
	}
	return "", false, false
}

// walkImportGraph BFS-expands the entry sources through their LOCAL
// imports. Dotted local imports resolve the WHOLE chain: every existing
// file along `pkg.helper` (pkg/__init__.py, pkg/helper.py) is scanned,
// and a missing link is a static layout finding — `import pkg.helper`
// requires pkg/helper to be a module, full stop.
func (p Preflight) walkImportGraph(ctx context.Context, d *discovery, workingDir string, entry []pySource) {
	visited := map[string]bool{}
	seenMod := map[string]bool{}
	seenLayout := map[string]bool{}
	queue := append([]pySource(nil), entry...)

	enqueue := func(file string) {
		if !visited[file] && len(visited)+len(queue) < maxSourceFiles {
			if src, err := p.fsys().ReadFile(file); err == nil {
				queue = append(queue, pySource{Path: file, Src: src})
			}
		}
	}

	for len(queue) > 0 {
		if ctx.Err() != nil || len(d.Scanned) >= maxSourceFiles {
			d.Truncated = true
			return
		}
		s := queue[0]
		queue = queue[1:]
		if visited[s.Path] {
			continue
		}
		visited[s.Path] = true
		d.Scanned = append(d.Scanned, s)

		for _, dotted := range parseImports(s.Src) {
			if seenMod[dotted] {
				continue
			}
			seenMod[dotted] = true
			parts := strings.Split(dotted, ".")
			head := parts[0]

			// Local iff the head resolves under workingDir.
			if _, _, ok := p.resolveModuleFile(workingDir, parts[:1]); !ok {
				if !seenMod["ext:"+head] {
					seenMod["ext:"+head] = true
					d.External = append(d.External, head)
				}
				continue
			}
			if !seenMod["loc:"+head] {
				seenMod["loc:"+head] = true
				d.LocalTop = append(d.LocalTop, head)
			}
			// Walk the dotted chain: scan every level that exists; a
			// missing level is a layout finding (python would fail the
			// same way — `from pkg.helper import x` needs the module).
			for i := 1; i <= len(parts); i++ {
				if ctx.Err() != nil {
					d.Truncated = true
					return
				}
				file, isPkg, ok := p.resolveModuleFile(workingDir, parts[:i])
				if !ok {
					prefix := strings.Join(parts[:i], ".")
					if !seenLayout[prefix] {
						seenLayout[prefix] = true
						d.LayoutFindings = append(d.LayoutFindings, PreflightFinding{
							Kind: "import",
							Detail: fmt.Sprintf(
								"local import %q: %s has no %s.py / %s/__init__.py under %s — check the import root / package layout",
								dotted, prefix, parts[i-1], parts[i-1], workingDir),
						})
					}
					break
				}
				enqueue(file)
				if !isPkg {
					break // a module file ends the chain
				}
			}
		}
	}
}
