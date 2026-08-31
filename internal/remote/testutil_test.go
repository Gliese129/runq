package remote

import (
	"context"
	"strings"

	"github.com/gliese129/runq-lab/internal/rfs"
)

// testFS wraps a real LocalFS for file operations but intercepts command
// execution, letting tests inject fake cluster responses (sbatch, qstat, etc.)
// without implementing the full rfs.FS interface from scratch.
type testFS struct {
	*rfs.LocalFS
	run func(ctx context.Context, command string) (string, error)
}

// newTestFSFromRunner creates a testFS that delegates file ops to the real OS
// and routes shell commands through the provided runner function.
func newTestFSFromRunner(run func(ctx context.Context, command string) (string, error)) *testFS {
	return &testFS{LocalFS: rfs.NewLocalFS(), run: run}
}

func (f *testFS) Exec(ctx context.Context, cmd string, args ...string) (stdout, stderr []byte, exitCode int, err error) {
	// shellRun always calls Exec("sh", "-c", command). Route those through the
	// fake runner; everything else falls through to real execution.
	if cmd == "sh" && len(args) >= 2 && args[0] == "-c" {
		out, rerr := f.run(ctx, args[1])
		if rerr != nil {
			return []byte(out), nil, 1, nil
		}
		return []byte(out), nil, 0, nil
	}
	return f.LocalFS.Exec(ctx, cmd, args...)
}

// schedCmd returns the LAST line of a command routed through the fake
// runner — the scheduler invocation itself. Submit commands now carry
// the shared env prelude (HOME/env_setup/exports, RQ-76 r2 #2) above
// the actual qsub/sbatch line, and the runner should match on the
// command, not the prelude.
func schedCmd(command string) string {
	lines := strings.Split(strings.TrimSpace(command), "\n")
	return lines[len(lines)-1]
}
