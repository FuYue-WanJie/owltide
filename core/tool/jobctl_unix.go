//go:build !windows

package tool

import (
	"os/exec"
	"syscall"
)

// setJobProcessGroup puts the job into its own process group (Linux/
// Unix) so killJobProcessGroup can kill the whole tree.
func setJobProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killJobProcessGroup kills the job's whole process group.
func killJobProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative pid targets the group; ignore the error (already dead).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// exitCode extracts the exit status from a Wait error.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Exited() {
				return ws.ExitStatus(), true
			}
		}
		return -1, true
	}
	return 0, false
}
