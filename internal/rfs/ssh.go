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
// network traffic until the first call.
//
// Thread safety: safe for concurrent use. sshConn serializes dial/reconnect;
// sftp.Client is documented as safe for concurrent use; SSH sessions are
// bounded by the semaphore in sshConn.
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

// Stat conn.getSFTP() → sc.Stat(path)
// sftp.Client.Stat already returns os.FileInfo (satisfies fs.FileInfo).
func (s *SSHFS) Stat(path string) (fs.FileInfo, error) {
	sc, err := s.conn.getSFTP()
	if err != nil {
		return nil, err
	}
	return sc.Stat(path)
}

// ReadFile conn.getSFTP() → sc.Open(path) → io.ReadAll(f) → f.Close()
func (s *SSHFS) ReadFile(path string) ([]byte, error) {
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

// Open conn.getSFTP() → sc.Open(path), wrap result in &sshFile{f}.
func (s *SSHFS) Open(path string) (File, error) {
	sc, err := s.conn.getSFTP()
	if err != nil {
		return nil, err
	}
	f, err := sc.Open(path)
	if err != nil {
		return nil, err
	}
	return &sshFile{f}, nil
}

// ReadDir conn.getSFTP() → sc.ReadDir(path)
// ⚠ sftp.ReadDir returns []os.FileInfo, need fs.FileInfoToDirEntry() per element.
func (s *SSHFS) ReadDir(path string) ([]fs.DirEntry, error) {
	sc, err := s.conn.getSFTP()
	if err != nil {
		return nil, err
	}
	fis, err := sc.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, 0, len(fis))
	for _, fi := range fis {
		de := fs.FileInfoToDirEntry(fi)
		entries = append(entries, de)
	}
	return entries, nil
}

// ── Write ───────────────────────────────────────────────────────────────────

// WriteFile conn.getSFTP() → sc.OpenFile(path, O_WRONLY|O_CREATE|O_TRUNC) →
//
//	f.Write(data) → f.Close() → sc.Chmod(path, perm)
//
// sftp has no atomic write; temp+rename is a future TODO.
func (s *SSHFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	sc, err := s.conn.getSFTP()
	if err != nil {
		return err
	}
	f, err := sc.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = sc.Chmod(path, perm)
	return nil
}

// MkdirAll conn.getSFTP() → sc.MkdirAll(path)
// sftp.MkdirAll ignores perm (uses remote umask). Chmod walk if needed later.
func (s *SSHFS) MkdirAll(path string, perm os.FileMode) error {
	sc, err := s.conn.getSFTP()
	if err != nil {
		return err
	}
	return sc.MkdirAll(path)
}

// ── Command ─────────────────────────────────────────────────────────────────

// Exec runs a remote command via SSH session.
//
// Steps:
//  1. conn.newSession() → sess, release; defer release(); defer sess.Close()
//  2. sess.Stdout / sess.Stderr = separate bytes.Buffer
//  3. sess.Run(buildCmdLine(cmd, args))
//  4. On error → check *ssh.ExitError:
//     - yes: return stdout, stderr, ee.ExitStatus(), nil  (non-zero exit ≠ Go error)
//     - no:  transport failure → return stdout, stderr, -1, err
//  5. Success → return stdout, stderr, 0, nil
//
// ctx cancellation (later): goroutine watching ctx.Done() → sess.Signal(SIGKILL) or sess.Close()
func (s *SSHFS) Exec(ctx context.Context, cmd string, args ...string) (stdout, stderr []byte, exitCode int, err error) {
	sess, release, err := s.conn.newSession()

	if err != nil {
		return nil, nil, -1, err
	}
	defer release()
	defer sess.Close()

	var buffo, buffe []byte
	sob := bytes.NewBuffer(buffo)
	seb := bytes.NewBuffer(buffe)
	sess.Stdout = sob
	sess.Stderr = seb

	go func() {
		<-ctx.Done()
		sess.Close()
	}()

	runErr := sess.Run(buildCmdLine(cmd, args))
	so := sob.Bytes()
	se := seb.Bytes()

	if runErr != nil {
		if ee, ok := runErr.(*ssh.ExitError); ok {
			return so, se, ee.ExitStatus(), nil
		}
		return so, se, -1, runErr
	}
	return so, se, 0, nil
}

// ExecStream runs a remote command, returns streaming io.ReadCloser of combined output.
//
// Steps:
//  1. conn.newSession() → sess, release
//  2. io.Pipe() → pr, pw; sess.Stdout = pw; sess.Stderr = pw
//  3. sess.Start(buildCmdLine(cmd, args))  — NOT Run (Start is non-blocking)
//  4. go func() { pw.CloseWithError(sess.Wait()); sess.Close(); release() }()
//  5. return pr, nil
//  6. On Start error → release + close sess/pw/pr, return error
func (s *SSHFS) ExecStream(ctx context.Context, cmd string, args ...string) (io.ReadCloser, error) {
	sess, release, err := s.conn.newSession()

	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()

	sess.Stdout = pw
	sess.Stderr = pw

	startErr := sess.Start(buildCmdLine(cmd, args))

	if startErr != nil {
		pr.Close()
		pw.Close()
		sess.Close()
		release()
		return nil, startErr
	}

	go func() {
		<-ctx.Done()
		sess.Close()
	}()

	go func() {
		pw.CloseWithError(sess.Wait())
		sess.Close()
		release()
	}()
	return pr, nil
}

// ── Transfer ────────────────────────────────────────────────────────────────

// CopyToLocal sftp download to local file.
//
// Steps: applyCopyOpts(opts) → conn.getSFTP() → sc.Open(remotePath) →
//
//	os.Create(localPath) → io.Copy(dst, src) → close both
func (s *SSHFS) CopyToLocal(remotePath, localPath string, opts ...CopyOption) error {
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
// This is the POSIX-safe quoting method: 'foo'\”bar' → foo'bar.
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

// sshFile wraps *sftp.File to satisfy rfs.File.
// All four methods are one-line delegates.
type sshFile struct {
	f *sftp.File
}

func (sf *sshFile) Read(p []byte) (int, error)           { return sf.f.Read(p) }
func (sf *sshFile) Seek(off int64, w int) (int64, error) { return sf.f.Seek(off, w) }
func (sf *sshFile) Close() error                         { return sf.f.Close() }
func (sf *sshFile) Stat() (fs.FileInfo, error)           { return sf.f.Stat() }

// compile-time interface check
var _ FS = (*SSHFS)(nil)

// suppress unused import for ssh (only used inside TODO bodies)
var _ = (*ssh.ExitError)(nil)
