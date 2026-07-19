package rfs

// Host key verification (RQ-45). Policy is split by SCENARIO:
//
//   - daemon lanes (background, no human): STRICT — unknown or mismatched
//     host keys refuse the connection with a typed error naming the fix.
//   - `runq connect` (interactive): TOFU with consent — the fingerprint is
//     shown, the human decides, an accepted key is appended to known_hosts
//     so every later strict connection just works.
//
// Both paths read/write the user's own ~/.ssh/known_hosts — runq is a
// normal SSH citizen, not a parallel trust store.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// UnknownHostKeyError: the host presented a key we have never seen.
type UnknownHostKeyError struct {
	Host        string
	Fingerprint string // SHA256:...
}

func (e *UnknownHostKeyError) Error() string {
	return fmt.Sprintf("unknown host key for %s (%s) — run `runq connect` to verify and trust it, or ssh once manually. To trust new hosts automatically, set `StrictHostKeyChecking accept-new` for this host in ~/.ssh/config (runq honors it)", e.Host, e.Fingerprint)
}

// HostKeyMismatchError: the host's key CHANGED. Deliberately alarming —
// this is either a server reinstall, a login-node pool member with its own
// key, or an interception. Never promptable and never auto-accepted (even
// under accept-new): the way out is a copy-paste command, not a click
// (RQ-74 — the error carries its own fix).
type HostKeyMismatchError struct {
	Host        string
	Fingerprint string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("HOST KEY MISMATCH for %s (now %s): the server's key differs from ~/.ssh/known_hosts — possible MITM; verify against the cluster's published fingerprints before trusting.\n"+
		"Common benign causes: the cluster was reinstalled/rotated its keys, or this hostname fronts a POOL of login nodes with differing keys (another pool member answered this time).\n"+
		"If verified, reset and re-trust with:\n"+
		"    ssh-keygen -R %s && runq connect\n"+
		"(Note: \"StrictHostKeyChecking accept-new\" in ~/.ssh/config auto-trusts NEW hosts only — a changed key always stops here.)",
		e.Host, e.Fingerprint, sshKeygenTarget(e.Host))
}

// sshKeygenTarget renders a host the way `ssh-keygen -R` expects it:
// bare hostname for port 22, bracketed [host]:port otherwise.
func sshKeygenTarget(host string) string {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if p == "22" {
		return h
	}
	return fmt.Sprintf("[%s]:%s", h, p)
}

// knownHostsFile returns ~/.ssh/known_hosts, creating an empty one (and
// ~/.ssh, with ssh-conventional permissions) if missing — a fresh machine
// should get "unknown host key", not "no such file".
func knownHostsFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "known_hosts")
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	f.Close()
	return path, nil
}

// StrictHostKeyCallback verifies against ~/.ssh/known_hosts and maps the
// library's KeyError into the two typed errors above.
func StrictHostKeyCallback() (ssh.HostKeyCallback, error) {
	path, err := knownHostsFile()
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	inner, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := inner(hostname, remote, key)
		if err == nil {
			return nil
		}
		var kerr *knownhosts.KeyError
		if errors.As(err, &kerr) {
			if len(kerr.Want) == 0 {
				return &UnknownHostKeyError{Host: hostname, Fingerprint: ssh.FingerprintSHA256(key)}
			}
			return &HostKeyMismatchError{Host: hostname, Fingerprint: ssh.FingerprintSHA256(key)}
		}
		return err
	}, nil
}

// policyHostKeyCallback maps a HostKeyPolicy to its callback — the shared
// "HostKeyCallback is nil" path of sshConn and RemoteForward.
func policyHostKeyCallback(policy string) (ssh.HostKeyCallback, error) {
	if policy == HostKeyAcceptNew {
		return AcceptNewHostKeyCallback()
	}
	return StrictHostKeyCallback()
}

// AcceptNewHostKeyCallback is the ssh_config `StrictHostKeyChecking
// accept-new` passthrough (RQ-74): an UNKNOWN host is trusted and recorded
// without a prompt — exactly what the user's own ssh would do under that
// setting, so runq is never more ceremonious than ssh. A MISMATCH still
// hard-fails: OpenSSH's `no` would tolerate even that, but silently
// accepting a CHANGED key disables the MITM protection entirely — runq
// caps the passthrough at accept-new semantics on purpose.
func AcceptNewHostKeyCallback() (ssh.HostKeyCallback, error) {
	strict, err := StrictHostKeyCallback()
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := strict(hostname, remote, key)
		var unknown *UnknownHostKeyError
		if !errors.As(err, &unknown) {
			return err // nil, mismatch, or IO error
		}
		return AppendKnownHost(hostname, key)
	}, nil
}

// TOFUHostKeyCallback wraps strict verification with a consent prompt for
// the UNKNOWN case only (mismatch is never promptable — that one is a
// security event, not an onboarding step). On consent the key is appended
// to known_hosts, making all future strict connections succeed.
func TOFUHostKeyCallback(confirm func(host, fingerprint string) bool) (ssh.HostKeyCallback, error) {
	strict, err := StrictHostKeyCallback()
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := strict(hostname, remote, key)
		var unknown *UnknownHostKeyError
		if !errors.As(err, &unknown) {
			return err // nil, mismatch, or IO error — never prompted away
		}
		if !confirm(hostname, unknown.Fingerprint) {
			return fmt.Errorf("host key for %s rejected by user", hostname)
		}
		return AppendKnownHost(hostname, key)
	}, nil
}

// AppendKnownHost appends one accepted host key to ~/.ssh/known_hosts in
// the standard format.
func AppendKnownHost(hostname string, key ssh.PublicKey) error {
	path, err := knownHostsFile()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	_, err = f.WriteString(line + "\n")
	return err
}
