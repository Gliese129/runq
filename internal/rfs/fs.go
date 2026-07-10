// Package rfs provides a unified filesystem abstraction for runq. LocalFS wraps
// os.* for same-machine operation; SSHFS wraps sftp + ssh.Session for remote
// clusters. The HPC backend receives an FS at construction and uses it for all
// file I/O and command execution, making local vs. remote transparent.
//
// Design decisions:
//   - No cache layer: reads go straight through. Incremental-read state (metric
//     offsets, log offsets) is maintained in-memory by the caller.
//   - Exec runs a command on the target machine. For SSHFS this means an SSH
//     session; for LocalFS it means os/exec. The caller is responsible for
//     quoting and escaping.
//   - CopyToLocal transfers a file from the FS to the local machine. For LocalFS
//     this is a plain file copy; for SSHFS it is an sftp download.
package rfs

import (
	"context"
	"io"
	"io/fs"
	"os"
)

// FS is the filesystem abstraction injected into the HPC backend. It covers
// file reads/writes, directory operations, command execution, and file
// transfer. Implementations: LocalFS (same machine) and SSHFS (remote).
type FS interface {
	// ── Read ──

	// Stat returns file metadata, like os.Stat.
	Stat(path string) (fs.FileInfo, error)

	// ReadFile reads the named file in full, like os.ReadFile.
	ReadFile(path string) ([]byte, error)

	// Open opens a seekable file handle for incremental reads.
	Open(path string) (File, error)

	// ReadDir reads a directory, like os.ReadDir.
	ReadDir(path string) ([]fs.DirEntry, error)

	// ── Write ──

	// WriteFile writes data to the named file, like os.WriteFile.
	WriteFile(path string, data []byte, perm os.FileMode) error

	// MkdirAll creates a directory tree, like os.MkdirAll.
	MkdirAll(path string, perm os.FileMode) error

	// ── Command ──

	// Exec runs a command on the target machine, returning stdout, stderr,
	// the exit code, and any transport-level error. A non-zero exit code is
	// NOT returned as an error — the caller decides how to interpret it.
	Exec(ctx context.Context, cmd string, args ...string) (stdout, stderr []byte, exitCode int, err error)

	// ExecStream runs a command and returns a stream of its combined output.
	// Used for `runq logs -f` style live tailing over SSH.
	ExecStream(ctx context.Context, cmd string, args ...string) (io.ReadCloser, error)

	// ── Transfer ──

	// CopyToLocal copies a file from this FS to a local path. For LocalFS
	// this is a file copy; for SSHFS it is an sftp download. The target
	// directory must already exist.
	CopyToLocal(remotePath, localPath string, opts ...CopyOption) error
}

// File is a seekable, closeable file handle returned by FS.Open.
type File interface {
	io.Reader
	io.Seeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// CopyOption configures CopyToLocal behavior.
type CopyOption func(*copyOpts)

type copyOpts struct {
	// Future options: progress callback, resume, bandwidth limit.
}

func applyCopyOpts(opts []CopyOption) copyOpts {
	var o copyOpts
	for _, fn := range opts {
		fn(&o)
	}
	return o
}
