package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	s.Now = func() time.Time { return base }
	return s
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAndLog(t *testing.T) {
	s := newTestStore(t)
	write(t, s.Dir, "config.yaml", "model: {}\n")
	id1, changed, err := s.AutoCommit(CommitMeta{Who: "user", Reason: "init"})
	if err != nil || !changed {
		t.Fatalf("first commit: %v changed=%v", err, changed)
	}
	id2, changed, err := s.AutoCommit(CommitMeta{Who: "agent", Tool: "skill.write", Reason: "add skill", Game: "s1"})
	if err != nil || changed {
		t.Fatalf("noop commit should report no change: %v changed=%v", err, changed)
	}
	write(t, s.Dir, "skills/hello.md", "# hi\n")
	id3, changed, err := s.AutoCommit(CommitMeta{Who: "agent", Tool: "skill.write", Reason: "add skill"})
	if err != nil || !changed {
		t.Fatalf("second commit: %v", err)
	}
	if id2 != "" && id2 != id1 {
		t.Fatalf("noop returned new id %s", id2)
	}
	log, err := s.Log()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("want 2 commits, got %d", len(log))
	}
	if log[0].ID != id3 || len(log[0].Changed) != 1 || log[0].Changed[0] != "skills/hello.md" {
		t.Fatalf("bad log head: %+v", log[0])
	}
	_, _, toolName, _ := ParseMessage(log[0].Msg)
	if toolName != "skill.write" {
		t.Fatalf("tool not parsed: %q (msg=%q)", toolName, log[0].Msg)
	}
}

func TestGitCompatHeader(t *testing.T) {
	s := newTestStore(t)
	write(t, s.Dir, "config.yaml", "a: 1\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "user", Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	// Object must decompress to "commit <len>\0..." format.
	head, err := s.Head()
	if err != nil || head == "" {
		t.Fatal("no head")
	}
	typ, payload, err := s.readObject(head)
	if err != nil || typ != "commit" {
		t.Fatalf("object type %s err %v", typ, err)
	}
	if len(payload) == 0 || payload[0] != 't' {
		t.Fatalf("bad payload start %q", payload[:20])
	}
}

func TestLocksPermanentAndPersistence(t *testing.T) {
	s := newTestStore(t)
	if !s.IsLocked("locks") || !s.IsLocked("config:budget") {
		t.Fatal("permanent locks missing")
	}
	if err := s.Lock("locks"); err == nil {
		t.Fatal("locking a permanent id must fail")
	}
	if err := s.Unlock("locks"); err == nil {
		t.Fatal("unlocking a permanent id must fail")
	}
	if err := s.Lock("agent:reviewer"); err != nil {
		t.Fatal(err)
	}
	if !s.IsLocked("agent:reviewer") {
		t.Fatal("lock not visible")
	}
	// Fresh store over the same dir sees the lock.
	s2, err := Open(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsLocked("agent:reviewer") {
		t.Fatal("lock not persisted")
	}
	if err := s2.Unlock("agent:reviewer"); err != nil {
		t.Fatal(err)
	}
	if s2.IsLocked("agent:reviewer") {
		t.Fatal("unlock failed")
	}
}

func TestCheckoutRevert(t *testing.T) {
	s := newTestStore(t)
	write(t, s.Dir, "config.yaml", "v: 1\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "user", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "config.yaml", "v: 2\n")
	write(t, s.Dir, "agents/a.yaml", "name: a\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "agent", Reason: "second"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevertLast("test"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, "config.yaml"))
	if string(data) != "v: 1\n" {
		t.Fatalf("revert did not restore: %q", data)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "agents/a.yaml")); !os.IsNotExist(err) {
		t.Fatal("revert did not remove added file")
	}
	_ = os.Remove
	if len(s.Dir) == 0 {
		t.Fatal("unreachable")
	}
}
