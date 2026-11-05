// Package proc starts and stops shell command process trees.
package proc

import (
	"os/exec"
	"time"
)

// prepareTimeout bounds how long Wait waits for inherited stdio pipes after the
// process was killed. Without it a killed shell can block on children that
// still hold the pipe open.
const prepareTimeout = 2 * time.Second

// Prepare places cmd in its own process group and installs a context
// cancellation hook that terminates the whole tree. Commands created with
// exec.Command (no context) keep their nil Cancel field.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
	cmd.WaitDelay = prepareTimeout
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return KillTree(cmd) }
	}
}
