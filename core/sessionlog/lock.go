package sessionlog

// Session lock: one .lock file per session directory, created with
// O_EXCL and holding the holder's pid. A second process opening the
// same session is refused while the holder is alive; a lock left by a
// dead process (crash) is reported stale and taken over. No cgo: the
// liveness probe uses the stdlib only (build-tagged per platform).

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SessionLock is an exclusive hold on one session directory.
type SessionLock struct {
	path string
	pid  int
}

// LockSession acquires the session lock under <root>/<sessionID>/.lock.
// Outcomes:
//   - fresh acquire: lock==..., stale=false, err=nil
//   - stale lock (holder pid is dead): the stale file is removed and
//     the lock re-acquired; stale=true (caller should tell the user)
//   - live lock: err names the holding pid (session open elsewhere)
func LockSession(root, sessionID string) (lock *SessionLock, stale bool, err error) {
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, ".lock")
	pid := os.Getpid()
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", pid)
			f.Close()
			return &SessionLock{path: path, pid: pid}, stale, nil
		}
		if !os.IsExist(err) {
			return nil, false, err
		}
		// Lock exists: decide live vs stale by the recorded pid.
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, false, fmt.Errorf(
				"session lock %s exists but is unreadable: %w", path, rerr)
		}
		holder, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr != nil || holder <= 0 {
			return nil, false, fmt.Errorf(
				"session lock %s exists but holds no valid pid; remove it manually if no other process is running this session", path)
		}
		if processAlive(holder) {
			return nil, false, fmt.Errorf(
				"session is open in another process (pid %d); refusing to double-open", holder)
		}
		// Stale: report once, take over.
		if attempt == 0 {
			stale = true
		}
		if err := os.Remove(path); err != nil {
			return nil, stale, fmt.Errorf(
				"stale session lock %s (pid %d is gone) could not be removed: %w", path, holder, err)
		}
	}
	return nil, stale, fmt.Errorf("session lock %s could not be acquired", path)
}

// Release removes the lock file, but only if it still records our own
// pid (never unlink a lock that was taken over by someone else).
func (l *SessionLock) Release() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	holder, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr == nil && holder != l.pid {
		return fmt.Errorf("session lock %s is held by pid %d; not releasing", l.path, holder)
	}
	err = os.Remove(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
