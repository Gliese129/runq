package rfs

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ResolveAuthMethods builds the ordered auth chain (RQ-45): ssh-agent
// first when SSH_AUTH_SOCK is present — signing stays in the agent, the
// daemon never reads key plaintext — then the configured key file. An
// encrypted key file without an agent is a clear, actionable error, not
// a parse failure.
func ResolveAuthMethods(keyPath string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			// conn lives as long as the process; the agent client owns it.
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		} else {
			slog.Warn("ssh-agent unreachable, falling back to key file", "error", err)
		}
	}

	if keyPath != "" {
		if fi, err := os.Stat(keyPath); err == nil && fi.Mode().Perm()&0o077 != 0 {
			slog.Warn("ssh key file permissions too open (ssh convention is 0600)", "key", keyPath, "mode", fi.Mode().Perm().String())
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read key %q: %w", keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			var passErr *ssh.PassphraseMissingError
			if errors.As(err, &passErr) {
				if len(methods) > 0 {
					// Agent is available — the encrypted file is fine to skip.
					slog.Debug("key file is passphrase-protected; relying on ssh-agent", "key", keyPath)
					return methods, nil
				}
				return nil, fmt.Errorf("key %q is passphrase-protected — add it to ssh-agent (`ssh-add %s`); the daemon does not prompt for passphrases", keyPath, keyPath)
			}
			return nil, fmt.Errorf("parse key %q: %w", keyPath, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable ssh auth: SSH_AUTH_SOCK not set and no key file configured")
	}
	return methods, nil
}
