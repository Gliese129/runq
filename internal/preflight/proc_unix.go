//go:build unix

package preflight

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup makes the probe shell a process-group leader and
// arranges for ctx cancellation to SIGKILL the WHOLE group — killing
// only the shell would leave python/extra_run grandchildren alive and
// holding the output pipe past the timeout.
func setupProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
