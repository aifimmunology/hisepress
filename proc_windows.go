//go:build windows

package hisepress

import "os/exec"

// configureProcess makes cancellation kill the command. Child processes the
// command started may survive on Windows.
func configureProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
}
