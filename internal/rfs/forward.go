package rfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"path"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ── Remote socket forward (RQ remote CLI) ───────────────────────────────────
//
// RemoteForward keeps a unix socket listening on a remote host (the HPC
// login node) and hands every accepted connection to a local server — the
// reverse of `ssh -R`, in-process. A `runq` CLI on the login node dials
// that socket and transparently talks to THIS machine's client daemon.
//
// It deliberately does NOT ride on sshConn: sshConn's contract is "look
// like a normal SSH user" (idle teardown after quiet periods), while a
// forward listener is a persistent service — holding the lane's connection
// open forever would defeat that etiquette. So the forward owns a second,
// dedicated connection with keepalives instead of idle teardown, and its
// own reconnect supervision.
//
// Security: the remote socket grants full control of the local daemon
// (which executes submit templates). The login node is multi-user, so the
// socket lives in a 0700 dir and is chmod'd 0600 after bind.

// RemoteForwardConfig configures a RemoteForward.
type RemoteForwardConfig struct {
	// SSH is the connection config. IdleTimeout is ignored — a forward
	// listener is persistent by definition (see package comment above).
	SSH SSHConfig

	// SocketPath is the remote unix socket path. Relative paths resolve
	// against the remote user's home (sftp session start dir).
	// Default: ".runq/runq.sock".
	SocketPath string

	// Serve consumes the remote listener and blocks until the listener
	// dies (the daemon passes http.Serve with the dashboard mux). It is
	// called once per established forward; a non-nil return is logged and
	// triggers a reconnect cycle.
	Serve func(net.Listener) error

	// OnEstablished, when set, is called once per established forward
	// session, right before Serve — the moment the remote socket is bound
	// and listening. The daemon uses it to record target reachability
	// (RQ-74): a forward coming up is transport-level proof of contact, so
	// doctor's "no contact yet" clears without waiting for the next
	// scheduled sensor pass. Must be fast and non-blocking.
	OnEstablished func()

	Logger *slog.Logger
}

// Forward reconnect/keepalive tuning.
const (
	forwardBackoffMin   = 2 * time.Second
	forwardBackoffMax   = time.Minute
	forwardHealthyAfter = 2 * time.Minute // session older than this resets backoff
	forwardKeepalive    = 30 * time.Second
)

// RemoteForward supervises one remote socket forward. Create with
// NewRemoteForward, drive with Run (blocking; run it in a goroutine),
// stop with Close.
type RemoteForward struct {
	cfg    RemoteForwardConfig
	logger *slog.Logger

	mu     sync.Mutex
	client *ssh.Client // current connection; nil between sessions

	closeOnce sync.Once
	done      chan struct{}
}

// NewRemoteForward creates a RemoteForward. No network traffic until Run.
func NewRemoteForward(cfg RemoteForwardConfig) *RemoteForward {
	if cfg.SocketPath == "" {
		cfg.SocketPath = ".runq/runq.sock"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &RemoteForward{cfg: cfg, logger: logger, done: make(chan struct{})}
}

// Run supervises the forward until ctx is cancelled or Close is called:
// dial → bind remote socket → Serve, then backoff and repeat. Failures are
// logged, never fatal — the HPC lane works with or without the forward.
func (f *RemoteForward) Run(ctx context.Context) {
	backoff := forwardBackoffMin
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		default:
		}

		started := time.Now()
		if err := f.session(ctx); err != nil {
			f.logger.Warn("remote socket forward down", "error", err)
		}
		if time.Since(started) > forwardHealthyAfter {
			backoff = forwardBackoffMin // it worked for a while; fail fast next time
		}

		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, forwardBackoffMax)
	}
}

// session runs one full forward lifetime: dial, prepare the socket dir,
// bind, serve until the connection or listener dies.
func (f *RemoteForward) session(ctx context.Context) error {
	hostKeys := f.cfg.SSH.HostKeyCallback
	if hostKeys == nil {
		var err error
		if hostKeys, err = StrictHostKeyCallback(); err != nil {
			return err
		}
	}
	client, err := ssh.Dial("tcp", f.cfg.SSH.addr(), &ssh.ClientConfig{
		User:            f.cfg.SSH.User,
		Auth:            f.cfg.SSH.AuthMethods,
		HostKeyCallback: hostKeys,
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	f.mu.Lock()
	select {
	case <-f.done: // Close raced the dial; don't leak the connection
		f.mu.Unlock()
		_ = client.Close()
		return nil
	default:
	}
	f.client = client
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.client = nil
		f.mu.Unlock()
		_ = client.Close()
	}()

	sockPath, err := f.prepareSocket(client)
	if err != nil {
		return err
	}

	ln, err := client.ListenUnix(sockPath)
	if err != nil {
		return fmt.Errorf("remote listen %s: %w", sockPath, err)
	}
	defer ln.Close()

	// The socket file is created by sshd under the remote umask — clamp it.
	// Best-effort: the 0700 parent dir is the real boundary.
	if sc, err := sftp.NewClient(client); err == nil {
		_ = sc.Chmod(sockPath, 0o600)
		_ = sc.Close()
	}

	// No exec traffic proves this connection alive, so probe it ourselves.
	// A dead transport makes SendRequest fail → close → Accept unblocks →
	// Serve returns → the Run loop reconnects.
	stopKeepalive := make(chan struct{})
	defer close(stopKeepalive)
	go func() {
		ticker := time.NewTicker(forwardKeepalive)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, _, err := client.SendRequest("keepalive@runq", true, nil); err != nil {
					_ = client.Close()
					return
				}
			case <-stopKeepalive:
				return
			case <-ctx.Done():
				_ = client.Close()
				return
			}
		}
	}()

	f.logger.Info("remote socket forward up", "host", f.cfg.SSH.Host, "socket", sockPath)
	if f.cfg.OnEstablished != nil {
		f.cfg.OnEstablished()
	}
	if err := f.cfg.Serve(ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// prepareSocket resolves the socket path against the remote home, ensures
// a 0700 parent dir, and removes a stale socket file — sshd refuses to
// bind over an existing one (OpenSSH StreamLocalBindUnlink defaults to no).
func (f *RemoteForward) prepareSocket(client *ssh.Client) (string, error) {
	sc, err := sftp.NewClient(client)
	if err != nil {
		return "", fmt.Errorf("sftp: %w", err)
	}
	defer sc.Close()

	sockPath := f.cfg.SocketPath
	if !path.IsAbs(sockPath) {
		home, err := sc.Getwd() // sftp session starts in the user's home
		if err != nil {
			return "", fmt.Errorf("resolve remote home: %w", err)
		}
		sockPath = path.Join(home, sockPath)
	}

	dir := path.Dir(sockPath)
	if err := sc.MkdirAll(dir); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// The socket grants control of the LOCAL daemon; on a multi-user login
	// node the parent dir permissions are the access boundary.
	if err := sc.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("chmod %s: %w", dir, err)
	}
	if err := sc.Remove(sockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("remove stale socket %s: %w", sockPath, err)
	}
	return sockPath, nil
}

// Close stops the supervisor and tears down the current connection (which
// unblocks Serve). Safe to call multiple times.
func (f *RemoteForward) Close() error {
	f.closeOnce.Do(func() {
		close(f.done)
		f.mu.Lock()
		if f.client != nil {
			_ = f.client.Close()
			f.client = nil
		}
		f.mu.Unlock()
	})
	return nil
}
