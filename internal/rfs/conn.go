package rfs

import (
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ── Config ──────────────────────────────────────────────────────────────────

// SSHConfig holds the parameters needed to establish an SSH connection.
// Populated from the HPC config section; see internal/hpcconfig.
type SSHConfig struct {
	Host       string // "login.cluster.edu" or "login.cluster.edu:22"
	User       string
	AuthMethod ssh.AuthMethod // from agent, key file, or password

	// IdleTimeout controls how long a connection survives without traffic
	// before sshConn tears it down. Zero means never idle-close.
	IdleTimeout time.Duration

	// MaxSessions caps the number of concurrent SSH sessions (Exec calls).
	// sftp runs on its own subsystem and does NOT consume a session slot.
	// Default: 3.
	MaxSessions int
}

// addr returns Host with ":22" appended if no port is present.
// Hint: strings.Contains(c.Host, ":") or net.SplitHostPort
func (c SSHConfig) addr() string {
	if _, _, err := net.SplitHostPort(c.Host); err != nil {
		return c.Host + ":22"
	}
	return c.Host
}

// ── Connection manager ──────────────────────────────────────────────────────

// sshConn is a lazy, reconnecting SSH connection manager.
//
// Design invariants:
//   - ONE persistent connection: dial on first use, reuse until idle or broken.
//   - sftp.Client rides on the same connection (its own SSH subsystem channel).
//   - Session concurrency bounded by sessionSem (cap = MaxSessions).
//   - Idle timeout: background goroutine tears down after IdleTimeout of inactivity.
//   - Reconnect: if getConn() finds client nil or dead, it re-dials.
type sshConn struct {
	mu         sync.Mutex
	client     *ssh.Client
	sftp       *sftp.Client
	lastUsed   time.Time
	cfg        SSHConfig
	sessionSem chan struct{} // cap = MaxSessions

	closeOnce sync.Once
	watchOnce sync.Once
	done      chan struct{} // closed by Close() to stop idle watcher

}

// newSSHConn creates a connection manager. Does NOT dial.
// Default MaxSessions to 3 if <= 0. Init sessionSem (buffered chan) and done chan.
func newSSHConn(cfg SSHConfig) *sshConn {
	maxCnt := cfg.MaxSessions
	if maxCnt <= 0 {
		maxCnt = 3
	}

	sc := sshConn{
		cfg:        cfg,
		lastUsed:   time.Now(),
		sessionSem: make(chan struct{}, maxCnt),
		done:       make(chan struct{}),
	}

	return &sc
}

// ── Public API (called by SSHFS) ────────────────────────────────────────────

// getConn returns a live *ssh.Client, dialing if needed. Thread-safe.
//
// Flow:
//  1. Lock mu (defer Unlock).
//  2. If client != nil → (optional) liveness check via
//     client.Conn.SendRequest("keepalive@openssh.com", true, nil) with short timeout.
//     If alive → touch lastUsed, return client.
//  3. If client nil or dead:
//     a. teardownLocked() to clean stale state
//     b. ssh.Dial("tcp", cfg.addr(), &ssh.ClientConfig{
//     User: cfg.User,
//     Auth: []ssh.AuthMethod{cfg.AuthMethod},
//     HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Phase 6: real verification
//     Timeout: 10 * time.Second,
//     })
//     c. sftp.NewClient(client)
//     d. Store both, touch lastUsed, call startIdleWatcher()
//  4. On dial failure → return error.
func (c *sshConn) getConn() (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client == nil {
		c.teardownLocked()
		client, err := ssh.Dial("tcp", c.cfg.addr(), &ssh.ClientConfig{
			User:            c.cfg.User,
			Auth:            []ssh.AuthMethod{c.cfg.AuthMethod},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
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
		c.lastUsed = time.Now()
		c.startIdleWatcher()
	}
	return c.client, nil
}

// getSFTP returns *sftp.Client. Call getConn() first (populates c.sftp),
// then return c.sftp.
func (c *sshConn) getSFTP() (*sftp.Client, error) {
	_, err := c.getConn()
	if err != nil || c.sftp == nil {
		return nil, err
	}
	return c.sftp, nil
}

// newSession opens an SSH session, blocking until a semaphore slot is free.
//
// Flow:
//  1. c.sessionSem <- struct{}{} — acquire slot (blocks if full)
//  2. getConn() → client; on error → <-sessionSem, return error
//  3. client.NewSession(); on error → <-sessionSem, return error
//  4. release = func() { <-c.sessionSem }
//  5. return sess, release, nil
//
// Caller MUST: defer release(); defer sess.Close()
func (c *sshConn) newSession() (sess *ssh.Session, release func(), err error) {
	c.sessionSem <- struct{}{}

	client, err := c.getConn()
	if err != nil {
		<-c.sessionSem
		return nil, nil, err
	}
	sess, err = client.NewSession()
	if err != nil {
		<-c.sessionSem
		return nil, nil, err
	}
	release = func() {
		<-c.sessionSem
	}
	return sess, release, nil
}

// ── Idle watcher ────────────────────────────────────────────────────────────

// startIdleWatcher launches goroutine that tears down after IdleTimeout inactivity.
// Must be called with mu held. Safe to call multiple times (only first spawns).
//
// If IdleTimeout <= 0 → no-op.
// Tick = IdleTimeout / 2. Each tick:
//
//	mu.Lock → if client != nil && time.Since(lastUsed) > IdleTimeout → teardownLocked()
//
// Exit on <-c.done.
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
					if c.client != nil && time.Since(c.lastUsed) > c.cfg.IdleTimeout {
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

// teardownLocked closes sftp then client, nils both. Must be called with mu held.
// Order matters: sftp first (it runs over the ssh conn), then client.
// Ignore close errors (best-effort cleanup).
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

// Close stops idle watcher and tears down connection. Safe to call multiple times.
// closeOnce.Do(func(){ close(done) }) → mu.Lock → teardownLocked → return nil
func (c *sshConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		c.teardownLocked()
		c.mu.Unlock()
	})
	return nil
}
