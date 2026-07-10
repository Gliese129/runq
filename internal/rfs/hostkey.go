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
	return fmt.Sprintf("unknown host key for %s (%s) — run `runq connect` to verify and trust it, or ssh once manually", e.Host, e.Fingerprint)
}

// HostKeyMismatchError: the host's key CHANGED. Deliberately alarming —
// this is either a server reinstall or an interception.
type HostKeyMismatchError struct {
	Host        string
	Fingerprint string
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("HOST KEY MISMATCH for %s (now %s): the server's key differs from ~/.ssh/known_hosts — possible MITM; verify out of band and update known_hosts manually", e.Host, e.Fingerprint)
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
