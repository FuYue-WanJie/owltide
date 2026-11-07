//go:build !unix

package sessionlog

// processAlive: no portable liveness probe on non-unix targets; any
// existing lock is treated as stale (windows is not a supported
// platform for OwlTide today).
func processAlive(pid int) bool {
	return false
}
