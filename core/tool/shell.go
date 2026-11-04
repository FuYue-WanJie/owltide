package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// shellArgs are run_shell parameters.
type shellArgs struct {
	Command string `json:"command"`
	// WorkingDir is optional; empty means inherit.
	WorkingDir string `json:"working_dir"`
}

// Shell runs a command through the platform shell.
// Timeout and output truncation protect the loop from runaway commands.
type Shell struct {
	// ShellBinary is the shell program (e.g. /bin/sh).
	ShellBinary string
	// ShellArg is the exec flag (e.g. -c).
	ShellArg string
	// TimeoutSec caps a single command; 0 means 60.
	TimeoutSec int
	// WorkDir is the default working directory.
	WorkDir string
}

// NewShell builds a Shell from platform defaults.
func NewShell(shellBinary, shellArg, workDir string, timeoutSec int) Shell {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	return Shell{ShellBinary: shellBinary, ShellArg: shellArg, TimeoutSec: timeoutSec, WorkDir: workDir}
}

func (Shell) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name:        "run_shell",
			Description: "Run a shell command and return combined stdout+stderr. Use for builds, tests, git, anything. Output is truncated if huge.",
			Parameters: object(
				`"command":{"type":"string"},"working_dir":{"type":"string"}`,
				"command"),
		},
	}
}

func (s Shell) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a shellArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.Command == "" {
		return "", fmt.Errorf("command is required")
	}
	dir := s.WorkDir
	if a.WorkingDir != "" {
		dir = a.WorkingDir
	}
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(s.TimeoutSec)*time.Second)
	defer cancel()

	var bin, flag string
	if runtime.GOOS == "windows" {
		bin, flag = "cmd", "/c"
	} else {
		bin, flag = "/bin/sh", "-c"
	}
	cmd := exec.CommandContext(cmdCtx, bin, flag, a.Command)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := clamp(buf.String())
	if err != nil {
		if ctxErr := cmdCtx.Err(); ctxErr != nil {
			return out, fmt.Errorf("timeout after %ds", s.TimeoutSec)
		}
		return out, fmt.Errorf("exit error: %w", err)
	}
	return out, nil
}
