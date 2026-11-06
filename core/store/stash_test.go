package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStashRecoveryAfterInterruptedRollback simulates a crash between
// checkout and restore: the journal exists, locked file on disk is the
// old version; RecoverStash must restore the locked content.
func TestStashRecoveryAfterInterruptedRollback(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnsureInit(); err != nil {
		t.Fatal(err)
	}
	// Agent v1 of a locked file.
	write(t, s.Dir, "agents/r.md", "name: r\nmodel: m1\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "agent", Reason: "v1"}); err != nil {
		t.Fatal(err)
	}
	// User upgrades it (v2) and locks it.
	write(t, s.Dir, "agents/r.md", "name: r\nmodel: m2\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "user", Reason: "v2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Lock("agent:r"); err != nil {
		t.Fatal(err)
	}
	v2, _ := os.ReadFile(filepath.Join(s.Dir, "agents/r.md"))

	// Begin a rollback: journal the stash (like evolve does), then
	// simulate the crash right after Checkout, before restore.
	protected := map[string][]byte{"agents/r.md": v2}
	if err := s.JournalLockedStash(protected, "s1"); err != nil {
		t.Fatal(err)
	}
	log, _ := s.Log()
	// log[0]=stash journal, log[1]=v2, log[2]=v1: checkout v1 commit.
	if err := s.Checkout(log[2].ID); err != nil {
		t.Fatal(err)
	}
	// Crash window: locked file is now the old v1 on disk.
	data, _ := os.ReadFile(filepath.Join(s.Dir, "agents/r.md"))
	if string(data) == string(v2) {
		t.Fatal("precondition: file should be the old version after crash")
	}

	// Restart: recovery must restore the locked v2 content.
	if err := s.RecoverStash(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(s.Dir, "agents/r.md"))
	if string(data) != string(v2) {
		t.Fatalf("locked file not restored: %q", data)
	}
	// Journal is cleared.
	if pending, _ := s.PendingStash(); pending {
		t.Fatal("stash not cleared after recovery")
	}
	// History records the recovery.
	log, _ = s.Log()
	found := false
	for _, c := range log {
		if stringsContains(c.Msg, "recover locked files") {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery commit missing from history")
	}
}

func stringsContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
