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
	// Background runs the command as a tracked job and returns a job
	// id at once (job_output / job_kill collect or stop it).
	Background bool `json:"background"`
}

// Caps: executes subprocesses (may also touch the network via
// commands; read-only roles get a parser-gated view instead).
func (*Shell) Caps() Caps { return CapExec }

// Shell runs a command through the platform shell.
// Timeout, output truncation, and the danger gate protect the loop.
type Shell struct {
	// ShellBinary is the shell program (e.g. /bin/sh).
	ShellBinary string
	// ShellArg is the exec flag (e.g. -c).
	ShellArg string
	// TimeoutSec caps a single command; 0 means 60.
	TimeoutSec int
	// WorkDir is the default working directory.
	WorkDir string
	// Jobs manages background=true runs; nil disables background.
	Jobs *JobManager

	confirmer       Confirmer
	detailConfirmer DetailConfirmer
}

// NewShell builds a Shell from platform defaults.
func NewShell(shellBinary, shellArg, workDir string, timeoutSec int) *Shell {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	return &Shell{ShellBinary: shellBinary, ShellArg: shellArg, TimeoutSec: timeoutSec, WorkDir: workDir}
}

// SetConfirmer wires the user-confirmation callback (exported for
// tests and custom wirings).
func (s *Shell) SetConfirmer(c Confirmer) { s.confirmer = c }

// setConfirmer implements confirmerHolder.
func (s *Shell) setConfirmer(c Confirmer) { s.SetConfirmer(c) }

// setDetailConfirmer implements detailConfirmerHolder.
func (s *Shell) setDetailConfirmer(c DetailConfirmer) { s.detailConfirmer = c }

// CloneWithConfirmer returns a copy of s with its own confirmer.
func (s *Shell) CloneWithConfirmer(c Confirmer) Tool {
	s2 := *s
	s2.confirmer = c
	s2.detailConfirmer = nil
	return &s2
}

func (Shell) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name: "run_shell",
			Description: "Execute a shell command. Returns combined stdout+stderr, " +
				"truncated if huge.",
			Parameters: object(
				`"command":{"type":"string"},`+
					`"working_dir":{"type":"string","description":"Optional working directory; empty inherits the session default."},`+
					`"background":{"type":"boolean","description":"Run asynchronously and return a job id immediately instead of waiting. Collect output with job_output; stop with job_kill."}`,
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
	if a.Background {
		if s.Jobs == nil {
			return "", fmt.Errorf("background execution is not enabled in this session")
		}
		if isDangerousCommand(a.Command) {
			if err := confirmDanger(s.confirmer, s.detailConfirmer, "command", a.Command); err != nil {
				return "", err
			}
		}
		dir := s.WorkDir
		if a.WorkingDir != "" {
			dir = a.WorkingDir
		}
		var bin, flag string
		if runtime.GOOS == "windows" {
			bin, flag = "cmd", "/c"
		} else {
			bin, flag = "/bin/sh", "-c"
		}
		id, err := s.Jobs.Start(bin, flag, a.Command, dir, JobTimeout)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{
			"job_id": id, "state": JobRunning,
			"note": "collect with job_output (offset cursor); stop with job_kill",
		})
		return string(b), nil
	}
	if isDangerousCommand(a.Command) {
		if err := confirmDanger(s.confirmer, s.detailConfirmer, "command", a.Command); err != nil {
			return "", err
		}
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
