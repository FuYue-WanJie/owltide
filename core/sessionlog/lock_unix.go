//go:build unix

package sessionlog

import "syscall"

// processAlive reports whether a process with the given pid exists
// (signal 0 probe; no cgo).
func processAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}
