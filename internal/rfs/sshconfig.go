package rfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ── ~/.ssh/config alias resolution ──────────────────────────────────────────
//
// `ssh tsubame` works because OpenSSH expands the alias through ssh_config;
// a runq target with `host: tsubame` must work the same way, or every user
// with SSH aliases gets "no such host" from a config that LOOKS right.
// So every SSH construction site (lane, remote forward, connect) runs the
// target's ssh settings through ResolveSSHConfigDefaults first.
//
// Semantics follow OpenSSH where it matters, simplified where it doesn't:
//   - For each parameter, the FIRST obtained value wins (OpenSSH rule);
//     options before any Host line apply to every host.
//   - Host patterns support * and ? wildcards and ! negation.
//   - Only HostName, User, Port, IdentityFile are read — that is what our
//     dialer consumes. Include and Match blocks are skipped (documented
//     limitation; `runq doctor` remains the place to debug ssh_config).
//   - Explicit target fields (yaml `user:`, `key:`, `port:`) are the
//     "command line" and always win over ssh_config values.

// ResolveSSHConfigDefaults fills the ZERO-valued fields of a target's SSH
// settings from ~/.ssh/config, treating host as an OpenSSH alias. The host
// itself is replaced by the block's HostName when one matches (that is the
// point of an alias); all other resolved values only fill gaps. Hosts that
// already carry a port ("host:22") are passed through untouched — they are
// addresses, not aliases. On any read/parse problem the inputs come back
// unchanged: resolution must never make a working config worse.
func ResolveSSHConfigDefaults(host string, port int, user, key string) (string, int, string, string) {
	if host == "" || strings.Contains(host, ":") {
		return host, port, user, key
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return host, port, user, key
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return host, port, user, key
	}
	v := resolveSSHAlias(string(data), host)

	if v.hostName != "" {
		host = v.hostName
	}
	if port == 0 && v.port != "" {
		if p, err := strconv.Atoi(v.port); err == nil && p > 0 && p < 65536 {
			port = p
		}
	}
	if user == "" {
		user = v.user
	}
	if key == "" && v.identityFile != "" {
		key = expandHome(v.identityFile, home)
	}
	return host, port, user, key
}

// sshConfigValues holds the subset of ssh_config parameters our dialer
// consumes. Empty string = not set by any matching block.
type sshConfigValues struct {
	hostName     string
	user         string
	port         string
	identityFile string
}

// resolveSSHAlias walks an ssh_config document and collects the effective
// values for alias, honoring first-obtained-wins across matching blocks.
func resolveSSHAlias(content, alias string) sshConfigValues {
	var v sshConfigValues
	active := true // options before the first Host line are global
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keyword, args := splitSSHConfigLine(line)
		switch keyword {
		case "host":
			active = hostPatternsMatch(args, alias)
			continue
		case "match":
			// Match conditions are not evaluated; skipping the block is
			// strictly safer than applying its options blindly.
			active = false
			continue
		}
		if !active || len(args) == 0 {
			continue
		}
		// First obtained value wins — only fill empty slots.
		switch keyword {
		case "hostname":
			if v.hostName == "" {
				v.hostName = args[0]
			}
		case "user":
			if v.user == "" {
				v.user = args[0]
			}
		case "port":
			if v.port == "" {
				v.port = args[0]
			}
		case "identityfile":
			if v.identityFile == "" {
				v.identityFile = args[0]
			}
		}
	}
	return v
}

// splitSSHConfigLine splits "Key value..." or "Key=value" into a lowercase
// keyword and its arguments, per ssh_config(5) syntax.
func splitSSHConfigLine(line string) (string, []string) {
	// "key=value" form: only the FIRST '=' is syntax.
	if i := strings.IndexAny(line, " \t="); i >= 0 && line[i] == '=' {
		return strings.ToLower(strings.TrimSpace(line[:i])), strings.Fields(line[i+1:])
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil
	}
	return strings.ToLower(fields[0]), fields[1:]
}

// hostPatternsMatch implements Host-line matching: any positive pattern
// must match, and no negated pattern may match. Wildcards * and ? follow
// fnmatch semantics (filepath.Match — hostnames contain no separators).
func hostPatternsMatch(patterns []string, alias string) bool {
	matched := false
	for _, pat := range patterns {
		negate := strings.HasPrefix(pat, "!")
		pat = strings.TrimPrefix(pat, "!")
		ok, err := filepath.Match(pat, alias)
		if err != nil || !ok {
			continue
		}
		if negate {
			return false
		}
		matched = true
	}
	return matched
}

// expandHome resolves the "~/" prefix ssh_config values commonly carry.
func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
