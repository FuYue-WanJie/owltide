package store

// Crash-safety journal for history.rollback. Before mutating the
// working tree, the current contents of locked files are committed
// under .lockstash/. If the process dies mid-rollback, RecoverStash
// restores them on next startup.

import (
	"os"
	"path/filepath"
	"strings"
)

const stashDir = ".lockstash"

// JournalLockedStash writes protected files (rel path -> bytes) into
// .lockstash/ and commits the journal. Fails when a previous journal
// is still pending (caller must recover first).
func (s *Store) JournalLockedStash(protected map[string][]byte, session string) error {
	if pending, _ := s.PendingStash(); pending {
		return errPendingStash
	}
	dir := filepath.Join(s.Dir, stashDir)
	os.RemoveAll(dir)
	for rel, data := range protected {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	_, _, err := s.AutoCommit(CommitMeta{
		Who: "agent", Tool: "lockstash", Game: session,
		Reason: "crash-safety stash for rollback",
	})
	return err
}

// errPendingStash guards against double-journaling.
var errPendingStash = errStashPending{}

type errStashPending struct{}

func (errStashPending) Error() string {
	return "pending lock stash exists; run recovery first"
}

// PendingStash reports whether .lockstash exists with content.
func (s *Store) PendingStash() (bool, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, stashDir))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// ClearLockedStash removes the journal directory and commits the removal.
func (s *Store) ClearLockedStash() error {
	dir := filepath.Join(s.Dir, stashDir)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	_, _, err := s.AutoCommit(CommitMeta{
		Who: "agent", Tool: "lockstash", Reason: "clear crash-safety stash",
	})
	return err
}

// RecoverStash completes an interrupted rollback: restores every
// journaled locked file over the working tree (agent mutations to
// locked paths never survive), commits, and clears the journal.
// Safe to call at startup whenever PendingStash is true.
func (s *Store) RecoverStash() error {
	dir := filepath.Join(s.Dir, stashDir)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	if err := restoreTree(dir, s.Dir); err != nil {
		return err
	}
	if _, _, err := s.AutoCommit(CommitMeta{
		Who: "agent", Tool: "lockstash",
		Reason: "recover locked files after interrupted rollback",
	}); err != nil {
		return err
	}
	return s.ClearLockedStash()
}

// restoreTree copies every regular file under src into dst, keeping
// relative paths (the journal mirrors the config layout).
func restoreTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := restoreTree(sp, dp); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(sp)
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(dp), 0o755)
		if err := os.WriteFile(dp, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

var _ = strings.TrimSpace
