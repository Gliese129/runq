package rfs

import (
	"strings"
	"testing"
)

// RQ-74: StrictHostKeyChecking parsing — accept-new and no/off collapse to
// HostKeyAcceptNew (runq caps `no` at accept-new semantics), everything
// else stays strict.
func TestResolveSSHAliasStrictHostKeyChecking(t *testing.T) {
	cfg := `
Host silky
    StrictHostKeyChecking accept-new
Host legacy
    StrictHostKeyChecking no
Host careful
    StrictHostKeyChecking ask
Host plain
    HostName plain.example
`
	cases := []struct {
		alias string
		want  string
	}{
		{"silky", "accept-new"},
		{"legacy", "no"},
		{"careful", "ask"},
		{"plain", ""},
	}
	for _, c := range cases {
		got := resolveSSHAlias(cfg, c.alias).strictHostKeyChecking
		if got != c.want {
			t.Errorf("alias %s: strictHostKeyChecking = %q, want %q", c.alias, got, c.want)
		}
	}
}

// The mismatch error is a self-contained fix (RQ-74): copyable ssh-keygen
// command with correct port bracketing, pool hint, accept-new boundary note.
func TestHostKeyMismatchErrorMessage(t *testing.T) {
	e := &HostKeyMismatchError{Host: "login.abci.ai:22", Fingerprint: "SHA256:abc"}
	msg := e.Error()
	for _, want := range []string{
		"ssh-keygen -R login.abci.ai", // port 22 → bare hostname
		"POOL of login nodes",
		"accept-new",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("mismatch message missing %q:\n%s", want, msg)
		}
	}

	e2 := &HostKeyMismatchError{Host: "login.cluster.edu:2222", Fingerprint: "SHA256:def"}
	if !strings.Contains(e2.Error(), "ssh-keygen -R [login.cluster.edu]:2222") {
		t.Errorf("non-22 port not bracketed:\n%s", e2.Error())
	}
}
