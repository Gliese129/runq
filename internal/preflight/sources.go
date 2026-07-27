package preflight

import (
	"path"
	"strings"
)

// Source discovery (Codex round 1 #2): import/HF analysis must follow the
// code the task will actually run, not just the first .py token —
//
//   - a `bash run.sh` entry is scanned for the python invocations INSIDE
//     the script (one level: run.sh → train.py), and
//   - local modules are walked as a dependency graph (train.py imports
//     helper → helper.py's own third-party imports count too),
//
// bounded by a visited set and a file cap so a pathological tree cannot
// turn preflight into a crawler (each remote ReadFile is a round trip).

// maxSourceFiles caps how many python files the graph walk reads.
const maxSourceFiles = 32

// pySource is one python file pulled into the analysis.
type pySource struct {
	Path string
	Src  []byte
}

// collectEntrySources resolves the entry command into its python sources:
// a .py entry directly, a .sh entry via the python invocations found in
// the script text. Returns the sources, the resolved .sh path (when the
// entry is a shell script), and the first read error for the ENTRY file
// (secondary reads are best-effort).
func (p Preflight) collectEntrySources(sampleCmd, workingDir string) (sources []pySource, shPath string, err error) {
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

// resolveLocalModule maps a top-level module name to the project file
// that defines it (name.py or name/__init__.py under workingDir), or "".
func (p Preflight) resolveLocalModule(workingDir, name string) string {
	file := path.Join(workingDir, name+".py")
	if _, err := p.fsys().Stat(file); err == nil {
		return file
	}
	pkg := path.Join(workingDir, name, "__init__.py")
	if _, err := p.fsys().Stat(pkg); err == nil {
		return pkg
	}
	return ""
}

// walkImportGraph BFS-expands the entry sources through their LOCAL
// imports and returns:
//
//   - external: third-party module heads across the whole graph, in
//     first-appearance order (these get probed), and
//   - scanned: every source read (entry + local deps) for HF extraction.
//
// Local modules are followed instead of silently dropped — the old
// behavior missed `train.py → helper.py → import missing_lib` entirely.
func (p Preflight) walkImportGraph(entry []pySource, workingDir string) (external []string, scanned []pySource) {
	visited := map[string]bool{}
	seenMod := map[string]bool{}
	queue := append([]pySource(nil), entry...)
	for len(queue) > 0 && len(scanned) < maxSourceFiles {
		s := queue[0]
		queue = queue[1:]
		if visited[s.Path] {
			continue
		}
		visited[s.Path] = true
		scanned = append(scanned, s)
		for _, mod := range parseImports(s.Src) {
			if seenMod[mod] {
				continue
			}
			seenMod[mod] = true
			if local := p.resolveLocalModule(workingDir, mod); local != "" {
				if !visited[local] && len(visited) < maxSourceFiles {
					if src, err := p.fsys().ReadFile(local); err == nil {
						queue = append(queue, pySource{Path: local, Src: src})
					}
				}
				continue // local: recurse, never probe (probing would execute it)
			}
			external = append(external, mod)
		}
	}
	return external, scanned
}
