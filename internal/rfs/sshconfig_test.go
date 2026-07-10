package rfs

import "testing"

const testSSHConfig = `
# global defaults (before any Host line)
IdentityFile ~/.ssh/global_key

Host tsubame
    HostName login.t4.gsic.titech.ac.jp
    User testuser
    IdentityFile ~/.ssh/id_ed25519
    Port 2222

Host abci abci-a
    HostName es.abci.local

Host *.cluster !bad.cluster
    User labuser

Host tsubame
    # second block for the same alias: first-obtained wins, so these lose
    User should-not-win
    HostName should-not-win.example

Match exec "true"
    HostName never-from-match.example

Host *
    User fallback
    Port=443
`

func TestResolveSSHAlias(t *testing.T) {
	cases := []struct {
		alias string
		want  sshConfigValues
	}{
		{"tsubame", sshConfigValues{
			hostName:     "login.t4.gsic.titech.ac.jp",
			user:         "testuser",
			port:         "2222",
			identityFile: "~/.ssh/global_key", // global line comes first — first-obtained wins
		}},
		{"abci-a", sshConfigValues{
			hostName:     "es.abci.local",
			user:         "fallback", // from Host *
			port:         "443",      // key=value form
			identityFile: "~/.ssh/global_key",
		}},
		{"gpu01.cluster", sshConfigValues{
			user:         "labuser",
			port:         "443",
			identityFile: "~/.ssh/global_key",
		}},
		{"bad.cluster", sshConfigValues{
			user:         "fallback", // negation knocks out the *.cluster block
			port:         "443",
			identityFile: "~/.ssh/global_key",
		}},
	}
	for _, c := range cases {
		got := resolveSSHAlias(testSSHConfig, c.alias)
		if got != c.want {
			t.Errorf("resolveSSHAlias(%q) = %+v, want %+v", c.alias, got, c.want)
		}
	}
}

func TestHostPatternsMatch(t *testing.T) {
	if hostPatternsMatch([]string{"*"}, "anything") != true {
		t.Error("* should match")
	}
	if hostPatternsMatch([]string{"tsu?ame"}, "tsubame") != true {
		t.Error("? should match one char")
	}
	if hostPatternsMatch([]string{"*.cluster", "!gpu01.cluster"}, "gpu01.cluster") != false {
		t.Error("negation should win")
	}
	if hostPatternsMatch([]string{"other"}, "tsubame") != false {
		t.Error("non-matching pattern should not match")
	}
}
