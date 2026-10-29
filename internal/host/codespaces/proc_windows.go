//go:build windows

package codespaces

import (
	"os/exec"
	"strconv"
)

// killTreeOnCancel kills gh and the ssh child it starts when the context ends:
// taskkill /T walks the process tree, which Windows has no process groups for.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		_ = cmd.Process.Kill() // in case taskkill could not run
		return nil
	}
}
