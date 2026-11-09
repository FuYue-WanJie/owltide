//go:build windows

package tool

import (
	"os/exec"
)

// setJobProcessGroup: Windows has no portable process groups in the
// stdlib; the job runs as a plain child (compiles, kills main process).
func setJobProcessGroup(cmd *exec.Cmd) {}

// killJobProcessGroup kills the job's main process.
func killJobProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// exitCode extracts the exit status from a Wait error.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), true
	}
	return 0, false
}
