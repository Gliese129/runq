//go:build !unix

package preflight

import "os/exec"

// setupProcessGroup is a no-op off unix; cmd.WaitDelay still bounds the
// post-cancel pipe wait.
func setupProcessGroup(_ *exec.Cmd) {}
