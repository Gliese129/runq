package rfs

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
)

// LocalFS implements FS using the local filesystem and os/exec.
// It is the default FS for same-machine (login-node) HPC operation.
type LocalFS struct{}

// NewLocalFS returns a LocalFS instance.
func NewLocalFS() *LocalFS { return &LocalFS{} }

// ── Read ──

func (l *LocalFS) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

func (l *LocalFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (l *LocalFS) Open(path string) (File, error) {
	return os.Open(path)
}

func (l *LocalFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path)
}

// ── Write ──

func (l *LocalFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// Rename is the optional atomic-replace extension (asserted by callers
// that write tmp-then-rename, e.g. the pyramid builder).
func (l *LocalFS) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (l *LocalFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// ── Command ──

func (l *LocalFS) Exec(ctx context.Context, cmd string, args ...string) (stdout, stderr []byte, exitCode int, err error) {
	c := exec.CommandContext(ctx, cmd, args...)
	var outBuf, errBuf bytes.Buffer
	c.Stdout = &outBuf
	c.Stderr = &errBuf

	runErr := c.Run()
	stdout = outBuf.Bytes()
	stderr = errBuf.Bytes()

	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return stdout, stderr, ee.ExitCode(), nil
		}
		// Transport-level error (binary not found, context cancelled, etc.)
		return stdout, stderr, -1, runErr
	}
	return stdout, stderr, 0, nil
}

func (l *LocalFS) ExecStream(ctx context.Context, cmd string, args ...string) (io.ReadCloser, error) {
	c := exec.CommandContext(ctx, cmd, args...)
	pr, pw := io.Pipe()
	c.Stdout = pw
	c.Stderr = pw

	if err := c.Start(); err != nil {
		pw.Close()
		pr.Close()
		return nil, err
	}

	go func() {
		pw.CloseWithError(c.Wait())
	}()

	return pr, nil
}

// ── Transfer ──

func (l *LocalFS) CopyToLocal(remotePath, localPath string, opts ...CopyOption) error {
	_ = applyCopyOpts(opts)

	src, err := os.Open(remotePath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Close()
}
