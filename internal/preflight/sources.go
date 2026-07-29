package preflight

import (
	"context"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/gliese129/runq/internal/rfs"
)

// Entry detection (Codex r3): Go's ONLY discovery job is finding what
// python invocation the command runs — the dependency graph itself is
// walked INSIDE the probe by CPython's own resolver (probe.py.tmpl).
// That leaves at most one Go-side remote read (the .sh entry text), and
// even that is ctx-bounded: rfs.FS has no context-aware read, so the
// read runs in a goroutine and a slow SFTP round trip cannot hold
// preflight past its deadline (the read finishes in the background).

// moduleEntryRegex catches `python -m pkg.mod` invocations.
var moduleEntryRegex = regexp.MustCompile(`(?:^|\s)python[\w.]*\s+(?:-[^m]\S*\s+)*-m\s+([A-Za-z_][\w.]*)`)

// writeFileCtx is readFileCtx's twin for probe uploads: a slow SFTP
// write must not hold preflight past its deadline either (Codex r4).
func writeFileCtx(ctx context.Context, fsys rfs.FS, p string, data []byte, perm os.FileMode) error {
	ch := make(chan error, 1)
	go func() { ch <- fsys.WriteFile(p, data, perm) }()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readFileCtx bounds fsys.ReadFile with ctx: the underlying transport
// has no cancellation, so the read continues in the background — but
// the CALLER returns at the deadline.
func readFileCtx(ctx context.Context, fsys rfs.FS, p string) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := fsys.ReadFile(p)
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// entrySpec is what the probe needs to know about the task's python
// entry point: mode "script" (Val = absolute .py path), "module"
// (Val = the -m dotted name), or "" (no python entry found).
type entrySpec struct {
	Mode string
	Val  string
	// ShPath is the resolved .sh entry (for bash -n), "" otherwise.
	// ShSrc is its text when read (interpreter detection).
	ShPath string
	ShSrc  string
}

// pythonEntryIn extracts a python invocation from command-ish text.
func pythonEntryIn(text, workingDir string) (mode, val string) {
	if m := moduleEntryRegex.FindStringSubmatch(text); len(m) >= 2 {
		return "module", m[1]
	}
	if m := scriptRegex.FindStringSubmatch(text); len(m) >= 2 {
		sp := m[1]
		if !strings.HasPrefix(sp, "/") {
			sp = path.Join(workingDir, sp)
		}
		return "script", sp
	}
	return "", ""
}

// detectEntry resolves the sample command into an entrySpec: a direct
// python entry, or a .sh entry whose text is scanned for the python it
// invokes (one level).
func (p Preflight) detectEntry(ctx context.Context, sampleCmd, workingDir string) (entrySpec, error) {
	if mode, val := pythonEntryIn(sampleCmd, workingDir); mode != "" {
		return entrySpec{Mode: mode, Val: val}, nil
	}
	shPath := shellEntry(sampleCmd, workingDir)
	if shPath == "" {
		return entrySpec{}, nil
	}
	src, err := readFileCtx(ctx, p.fsys(), shPath)
	if err != nil {
		return entrySpec{ShPath: shPath}, err
	}
	e := entrySpec{ShPath: shPath, ShSrc: string(src)}
	e.Mode, e.Val = pythonEntryIn(e.ShSrc, workingDir)
	return e, nil
}
