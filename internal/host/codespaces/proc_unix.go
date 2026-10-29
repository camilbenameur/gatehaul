//go:build unix

package codespaces

import (
	"errors"
	"os/exec"
	"syscall"
)

// killTreeOnCancel puts gh in its own process group and, when the context
// ends, kills the whole group: gh starts an ssh child that would otherwise
// outlive it and keep the connection open.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
