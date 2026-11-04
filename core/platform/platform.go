// Package platform abstracts OS differences so the core can run
// anywhere Go compiles, including Android (spawned as a subprocess).
package platform

import (
	"os"
	"path/filepath"
	"runtime"
)

// Platform collects the environment-specific knobs in one place.
// Desktop uses the default implementation; future Android builds
// will provide their own via a constructor switch.
type Platform interface {
	// Home returns the user's home directory.
	Home() string
	// TempDir returns a directory for temporary files.
	TempDir() string
	// Shell returns the shell binary used by run_shell.
	Shell() string
	// ShellFlag returns the flag that makes Shell execute a command string.
	ShellFlag() string
	// Name is a short identifier for logs ("desktop", "android", ...).
	Name() string
}

// Desktop is the default Platform for ordinary POSIX/Windows systems.
type Desktop struct{}

// New returns the platform matching the current runtime.
func New() Platform {
	return Desktop{}
}

// Home implements Platform.
func (Desktop) Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// TempDir implements Platform.
func (Desktop) TempDir() string { return os.TempDir() }

// Shell implements Platform.
func (Desktop) Shell() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "/bin/sh"
}

// ShellFlag implements Platform.
func (Desktop) ShellFlag() string {
	if runtime.GOOS == "windows" {
		return "/c"
	}
	return "-c"
}

// Name implements Platform.
func (Desktop) Name() string { return runtime.GOOS }

// ConfigDir returns ~/.owltide for the given home.
func ConfigDir(home string) string {
	return filepath.Join(home, ".owltide")
}
