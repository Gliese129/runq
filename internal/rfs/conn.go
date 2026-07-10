package rfs

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ── Config ──────────────────────────────────────────────────────────────────

// SSHConfig holds the parameters needed to establish an SSH connection.
// Populated from the target config (see internal/config.TargetConfig).
type SSHConfig struct {
	Host string // "login.cluster.edu" or "login.cluster.edu:22"
	User string
	// AuthMethods are tried in order. Convention (RQ-45): ssh-agent first
	// (the daemon never touches key plaintext), key file second. Build
	// with ResolveAuthMethods.
	AuthMethods []ssh.AuthMethod
	// HostKeyCallback: nil = STRICT known_hosts verification (the daemon
	// default). `runq connect` passes a TOFU callback with a human on the
	// other end. InsecureIgnoreHostKey never appears in this codebase.
	HostKeyCallback ssh.HostKeyCallback

	// IdleTimeout controls how long a connection survives with no in-flight
	// operations before sshConn tears it down. Zero means never idle-close.
	// Callers that want the "look like a normal SSH user" behavior MUST set
	// this (the daemon does; see backend.NewSSHBackend).
	IdleTimeout time.Duration

	// MaxSessions caps the number of concurrent SSH sessions (Exec calls).
	// sftp runs on its own subsystem and does NOT consume a session slot.
	// Default: 3.
	MaxSessions int
}

// addr returns Host with ":22" appended if no port is present.
func (c SSHConfig) addr() string {
	if _, _, err := net.SplitHostPort(c.Host); err != nil {
		return c.Host + ":22"
	}
	return c.Host
}

// livenessCheckAfter: on reuse, probe the connection only when it has been
// quiet for at least this long — recent successful traffic implies healthy,
// and probing every call would add a round trip to every operation.
const livenessCheckAfter = 30 * time.Second

// livenessTimeout bounds the keepalive probe. A half-open TCP connection
// (network partition) can block SendRequest for minutes; we declare the
// connection dead after this and redial. The probe goroutine unblocks and
// exits whenever the kernel finally gives up on the socket.
const livenessTimeout = 5 * time.Second

// errClosed is returned for operations on a Close()d connection manager.
var errClosed = errors.New("ssh connection manager closed")

// ── Connection manager ──────────────────────────────────────────────────────

// sshConn is a lazy, reconnecting SSH connection manager.
//
// Design invariants:
//   - ONE persistent connection: dial on first use, reuse while healthy.
//   - Liveness: a reused connection that has been quiet is probed with a
//     keepalive; a dead one is torn down and redialed transparently.
//   - sftp.Client rides on the same connection (its own SSH subsystem).
//   - Session concurrency bounded by sessionSem (cap = MaxSessions);
//     acquisition respects ctx cancellation and manager shutdown.
//   - Idle teardown: after IdleTimeout with ZERO in-flight operations
//     (active == 0), the watcher closes the connection. In-flight streams
//     (logs -f, large SFTP copies) hold active > 0 and are never cut.
type sshConn struct {
	mu       sync.Mutex
	client   *ssh.Client
	sftp     *sftp.Client
	lastUsed time.Time
	active   int // in-flight operations; idle teardown requires active == 0

	cfg        SSHConfig
	sessionSem chan struct{} // cap = MaxSessions

	closeOnce sync.Once
	watchOnce sync.Once
	done      chan struct{} // closed by Close() to stop idle watcher + waiters
}

// newSSHConn creates a connection manager. Does NOT dial.
func newSSHConn(cfg SSHConfig) *sshConn {
	maxCnt := cfg.MaxSessions
	if maxCnt <= 0 {
		maxCnt = 3
	}
	return &sshConn{
		cfg:        cfg,
		lastUsed:   time.Now(),
		sessionSem: make(chan struct{}, maxCnt),
		done:       make(chan struct{}),
	}
}

// ── Operation tracking ──────────────────────────────────────────────────────

// beginOp marks an operation in flight. Every public SSHFS operation is
// bracketed by beginOp/endOpErr; for handle-returning operations (Open,
// ExecStream) the bracket extends to the handle's Close, so the idle watcher
// can never cut a live stream.
//
// Deliberately does NOT touch lastUsed: lastUsed means "last moment the
// transport PROVED itself alive", and merely attempting an operation proves
// nothing. If beginOp refreshed it, a stream of failing calls against a dead
// connection would keep resetting the quiet-time clock and the keepalive
// probe in getConn would never fire — the connection could never self-heal.
func (c *sshConn) beginOp() {
	c.mu.Lock()
	c.active++
	c.mu.Unlock()
}

// endOp is endOpErr for paths without an error to classify.
func (c *sshConn) endOp() { c.endOpErr(nil) }

// endOpErr marks an operation finished. lastUsed is refreshed only when the
// outcome proves the transport worked: success, or a REMOTE verdict like
// not-exist/permission (the server answered — that IS a live round trip).
// Transport-level failures leave lastUsed alone, letting quiet time
// accumulate until getConn's keepalive probe fires and heals the connection.
func (c *sshConn) endOpErr(err error) {
	c.mu.Lock()
	c.active--
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		c.lastUsed = time.Now()
	}
	c.mu.Unlock()
}

// ── Connection acquisition ──────────────────────────────────────────────────

// getConn returns a live *ssh.Client, dialing or redialing as needed.
// Thread-safe. On reuse after a quiet period, the connection's liveness is
// verified first — a dead connection is torn down and redialed instead of
// poisoning every future call.
func (c *sshConn) getConn() (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.done:
		return nil, errClosed
	default:
	}

	// Reuse path: probe when the connection has been quiet.
	if c.client != nil && time.Since(c.lastUsed) > livenessCheckAfter {
		if !c.aliveLocked() {
			c.teardownLocked()
		}
	}

	if c.client == nil {
		hostKeys := c.cfg.HostKeyCallback
		if hostKeys == nil {
			var err error
			if hostKeys, err = StrictHostKeyCallback(); err != nil {
				return nil, err
			}
		}
		client, err := ssh.Dial("tcp", c.cfg.addr(), &ssh.ClientConfig{
			User:            c.cfg.User,
			Auth:            c.cfg.AuthMethods,
			HostKeyCallback: hostKeys,
			Timeout:         10 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		sftpClient, err := sftp.NewClient(client)
		if err != nil {
			_ = client.Close()
			return nil, err
		}
		c.client = client
		c.sftp = sftpClient
		c.lastUsed = time.Now() // a successful dial IS proof of life
		c.startIdleWatcher()
	}
	return c.client, nil
}

// aliveLocked probes the connection with a bounded keepalive round trip.
// Must be called with mu held. A live server answers (even a "request type
// unknown" reply proves the transport); a dead or half-open connection
// errors or times out. The probe goroutine leaks only until the kernel
// abandons the dead socket — bounded, and only at reconnect-worthy moments.
func (c *sshConn) aliveLocked() bool {
	ch := make(chan error, 1)
	client := c.client
	go func() {
		_, _, err := client.SendRequest("keepalive@runq", true, nil)
		ch <- err
	}()
	select {
	case err := <-ch:
		return err == nil
	case <-time.After(livenessTimeout):
		return false
	}
}

// getSFTP returns the *sftp.Client, (re)dialing via getConn as needed.
func (c *sshConn) getSFTP() (*sftp.Client, error) {
	if _, err := c.getConn(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sftp == nil {
		return nil, errClosed
	}
	return c.sftp, nil
}

// newSession opens an SSH session, blocking until a semaphore slot is free,
// the ctx is cancelled, or the manager is closed. Hung remote commands can
// exhaust the slots; ctx keeps callers (and shutdown) from hanging with them.
//
// Caller MUST: defer release(); defer sess.Close()
func (c *sshConn) newSession(ctx context.Context) (sess *ssh.Session, release func(), err error) {
	select {
	case c.sessionSem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-c.done:
		return nil, nil, errClosed
	}

	client, err := c.getConn()
	if err != nil {
		<-c.sessionSem
		return nil, nil, err
	}
	sess, err = client.NewSession()
	if err != nil {
		// NewSession can fail because the connection died (must redial) OR
		// because the server hit its per-connection session limit (healthy —
		// tearing down would cut every in-flight SFTP operation). Probe to
		// tell them apart and only tear down a genuinely dead connection.
		c.mu.Lock()
		if c.client == client && !c.aliveLocked() {
			c.teardownLocked()
		}
		c.mu.Unlock()
		<-c.sessionSem
		return nil, nil, err
	}
	var once sync.Once
	release = func() {
		once.Do(func() { <-c.sessionSem })
	}
	return sess, release, nil
}

// ── Idle watcher ────────────────────────────────────────────────────────────

// startIdleWatcher launches the goroutine that tears down the connection
// after IdleTimeout with no in-flight operations. Must be called with mu
// held. Only the first call spawns; the watcher survives teardown/redial
// cycles. No-op when IdleTimeout <= 0.
func (c *sshConn) startIdleWatcher() {
	if c.cfg.IdleTimeout <= 0 {
		return
	}
	c.watchOnce.Do(func() {
		ticker := time.NewTicker(c.cfg.IdleTimeout / 2)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					c.mu.Lock()
					if c.client != nil && c.active == 0 && time.Since(c.lastUsed) > c.cfg.IdleTimeout {
						c.teardownLocked()
					}
					c.mu.Unlock()
				case <-c.done:
					return
				}
			}
		}()
	})
}

// teardownLocked closes sftp then client, nils both. Must be called with mu
// held. Order matters: sftp rides on the ssh conn. Best-effort cleanup.
func (c *sshConn) teardownLocked() {
	if c.sftp != nil {
		_ = c.sftp.Close()
		c.sftp = nil
	}
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
}

// ── Shutdown ────────────────────────────────────────────────────────────────

// Close stops the idle watcher, unblocks session waiters, and tears down the
// connection. Safe to call multiple times.
func (c *sshConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		c.teardownLocked()
		c.mu.Unlock()
	})
	return nil
}
