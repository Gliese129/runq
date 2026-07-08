package rfs

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SSHFS implements FS over an SSH connection. File operations go through sftp;
// command execution opens SSH sessions. The underlying *sshConn is lazy — no
// network traffic until the first call — and self-healing: dead connections
// are detected and redialed transparently (see sshConn.getConn).
//
// Thread safety: safe for concurrent use. sshConn serializes dial/reconnect;
// sftp.Client is documented as safe for concurrent use; SSH sessions are
// bounded by the semaphore in sshConn.
//
// Every operation is bracketed by beginOp/endOp so the idle watcher never
// tears down a connection with work in flight; for handle-returning calls
// (Open, ExecStream) the bracket extends until the handle is closed.
type SSHFS struct {
	conn *sshConn
}

// NewSSHFS creates an SSHFS. The connection is lazy — no dial happens here.
func NewSSHFS(cfg SSHConfig) *SSHFS {
	return &SSHFS{conn: newSSHConn(cfg)}
}

// Close tears down the SSH connection. The SSHFS should not be used after Close.
func (s *SSHFS) Close() error {
	return s.conn.Close()
}

// ── Read ────────────────────────────────────────────────────────────────────

// Stat returns file info via sftp.
//
// All read/write methods below use the named-error + endOpErr pattern: the
// connection's proof-of-life clock only advances on outcomes that show the
// transport worked (incl. remote not-exist/permission verdicts), so a dead
// connection accumulates quiet time and gets probed/redialed by getConn.
func (s *SSHFS) Stat(path string) (fi fs.FileInfo, err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return nil, err
	}
	return sc.Stat(path)
}

// Rename is the optional atomic-replace extension (sftp PosixRename
// where servers support it would be stricter; plain Rename is what the
// tmp-then-rename writers need).
func (s *SSHFS) Rename(oldPath, newPath string) (err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return err
	}
	return sc.Rename(oldPath, newPath)
}

// Home returns the remote user's home directory (sftp Getwd — the
// protocol's session start dir is the user home). Optional interface: the
// fs browser uses it as the default start point.
func (s *SSHFS) Home() (dir string, err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return "", err
	}
	return sc.Getwd()
}

// ReadFile reads a whole remote file via sftp.
func (s *SSHFS) ReadFile(path string) (data []byte, err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return nil, err
	}
	f, err := sc.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Open returns a seekable remote file handle. The connection counts as busy
// until the returned File is closed — long incremental reads are never cut
// by the idle watcher.
func (s *SSHFS) Open(path string) (File, error) {
	s.conn.beginOp()
	sc, err := s.conn.getSFTP()
	if err != nil {
		s.conn.endOpErr(err)
		return nil, err
	}
	f, err := sc.Open(path)
	if err != nil {
		s.conn.endOpErr(err)
		return nil, err
	}
	return &sshFile{f: f, release: s.conn.endOp}, nil
}

// ReadDir lists a remote directory via sftp.
func (s *SSHFS) ReadDir(path string) (entries []fs.DirEntry, err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return nil, err
	}
	fis, err := sc.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries = make([]fs.DirEntry, 0, len(fis))
	for _, fi := range fis {
		entries = append(entries, fs.FileInfoToDirEntry(fi))
	}
	return entries, nil
}

// ── Write ───────────────────────────────────────────────────────────────────

// WriteFile writes data to a remote file (create/truncate), then chmods it.
// sftp has no atomic write; temp+rename is a future TODO.
func (s *SSHFS) WriteFile(path string, data []byte, perm os.FileMode) (err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return err
	}
	f, err := sc.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // don't leak the SFTP handle on a failed write
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = sc.Chmod(path, perm)
	return nil
}

// MkdirAll creates a remote directory tree.
// sftp.MkdirAll ignores perm (uses remote umask). Chmod walk if needed later.
func (s *SSHFS) MkdirAll(path string, perm os.FileMode) (err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	sc, err := s.conn.getSFTP()
	if err != nil {
		return err
	}
	return sc.MkdirAll(path)
}

// ── Command ─────────────────────────────────────────────────────────────────

// Exec runs a remote command via an SSH session and returns stdout, stderr,
// and the exit code. A non-zero exit is NOT a Go error; transport failures
// are. ctx cancellation closes the session (the remote command gets EOF/HUP).
func (s *SSHFS) Exec(ctx context.Context, cmd string, args ...string) (stdout, stderr []byte, exitCode int, err error) {
	s.conn.beginOp()
	// err carries transport failures only (non-zero exits return err == nil),
	// which is exactly the classification endOpErr wants.
	defer func() { s.conn.endOpErr(err) }()

	sess, release, err := s.conn.newSession(ctx)
	if err != nil {
		return nil, nil, -1, err
	}
	defer release()
	defer sess.Close()

	var sob, seb bytes.Buffer
	sess.Stdout = &sob
	sess.Stderr = &seb

	// ctx watcher with a guaranteed exit path: `finished` is closed when Run
	// returns, so this goroutine never outlives the call (a bare <-ctx.Done()
	// wait would leak one goroutine per call under a long-lived ctx).
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-finished:
		}
	}()

	runErr := sess.Run(buildCmdLine(cmd, args))
	close(finished)

	so, se := sob.Bytes(), seb.Bytes()
	if runErr != nil {
		if ee, ok := runErr.(*ssh.ExitError); ok {
			return so, se, ee.ExitStatus(), nil
		}
		return so, se, -1, runErr
	}
	return so, se, 0, nil
}

// ExecStream runs a remote command and returns a streaming reader of its
// combined output. The session, semaphore slot, and busy-marker are released
// when the command exits (or ctx is cancelled); the pipe's read side reports
// the command's exit status via CloseWithError.
func (s *SSHFS) ExecStream(ctx context.Context, cmd string, args ...string) (io.ReadCloser, error) {
	s.conn.beginOp()

	sess, release, err := s.conn.newSession(ctx)
	if err != nil {
		s.conn.endOpErr(err)
		return nil, err
	}

	pr, pw := io.Pipe()
	sess.Stdout = pw
	sess.Stderr = pw

	if err := sess.Start(buildCmdLine(cmd, args)); err != nil {
		pr.Close()
		pw.Close()
		sess.Close()
		release()
		s.conn.endOpErr(err)
		return nil, err
	}

	// Same watcher pattern as Exec: exits when the stream finishes.
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-finished:
		}
	}()
	go func() {
		pw.CloseWithError(sess.Wait())
		sess.Close()
		release()
		close(finished)
		s.conn.endOp()
	}()
	return pr, nil
}

// ── Transfer ────────────────────────────────────────────────────────────────

// CopyToLocal downloads a remote file via sftp. The busy-marker covers the
// whole transfer — large checkpoint downloads are never cut by idle teardown.
func (s *SSHFS) CopyToLocal(remotePath, localPath string, opts ...CopyOption) (err error) {
	s.conn.beginOp()
	defer func() { s.conn.endOpErr(err) }()
	_ = applyCopyOpts(opts) // for future
	sc, err := s.conn.getSFTP()
	if err != nil {
		return err
	}
	srcF, err := sc.Open(remotePath)
	if err != nil {
		return err
	}
	defer srcF.Close()

	dstF, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer dstF.Close()

	if _, err := io.Copy(dstF, srcF); err != nil {
		return err
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildCmdLine builds a command string for ssh.Session.Run.
//
// With no args, cmd is sent as-is (it's a pre-built shell command like
// "squeue -j 12345" that the remote shell should parse directly).
//
// With args, each token is shell-quoted so the remote sshd's shell
// word-splitting reconstructs the original argv. Without this,
// Exec("sh", "-c", "sbatch run.sh") would arrive as three words
// (sh / -c / sbatch) instead of two (sh -c / "sbatch run.sh").
func buildCmdLine(cmd string, args []string) string {
	if len(args) == 0 {
		return cmd
	}
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, shellQuote(cmd))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// sshFile wraps *sftp.File to satisfy rfs.File. Closing it also releases the
// connection's busy-marker taken at Open (idempotent).
type sshFile struct {
	f       *sftp.File
	release func()
}

func (sf *sshFile) Read(p []byte) (int, error)           { return sf.f.Read(p) }
func (sf *sshFile) Seek(off int64, w int) (int64, error) { return sf.f.Seek(off, w) }
func (sf *sshFile) Stat() (fs.FileInfo, error)           { return sf.f.Stat() }

func (sf *sshFile) Close() error {
	if sf.release != nil {
		sf.release()
		sf.release = nil
	}
	return sf.f.Close()
}

// compile-time interface check
var _ FS = (*SSHFS)(nil)
