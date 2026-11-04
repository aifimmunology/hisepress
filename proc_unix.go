//go:build !windows

package hisepress

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcess puts the command in its own process group and makes
// cancellation kill the whole group, so a timed-out script's children don't
// outlive it.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
