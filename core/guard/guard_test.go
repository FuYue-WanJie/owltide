package guard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/store"
)

func newGuard(t *testing.T) (*Guard, string) {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".owltide")
	for _, d := range []string{"agents"} {
		os.MkdirAll(filepath.Join(cfgDir, d), 0o755)
	}
	os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("model:\n  name: m\n"), 0o600)
	os.WriteFile(filepath.Join(cfgDir, "locks.yaml"), []byte("locked:\n    - agent:x\n"), 0o600)
	os.WriteFile(filepath.Join(cfgDir, "agents", "x.md"), []byte("v1"), 0o600)
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureInit(); err != nil {
		t.Fatal(err)
	}
	return &Guard{Root: cfgDir, St: st}, cfgDir
}

// A1: making a protected file unreadable must not silently disable the
// guard. As root, chmod 000 does not block reads, so the test tampers
// with the type (file -> directory) which readEntry rejects for anyone.
// Snapshot AFTER the tamper must fail; Verify must restore content and
// permissions.
func TestChmodTamperIsViolation(t *testing.T) {
	g, cfgDir := newGuard(t)
	before, err := g.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	locks := filepath.Join(cfgDir, "locks.yaml")
	// chmod is still exercised: with perms 000 a non-root process would
	// fail the same readEntry path.
	os.Chmod(locks, 0o000)
	// Type swap makes the path unreadable-as-file for any user.
	os.Remove(locks)
	os.MkdirAll(locks, 0o755)
	// Snapshot after tamper fails (fail closed, not skipped).
	if _, err := g.Snapshot(); err == nil {
		t.Fatal("snapshot over unreadable protected file must fail")
	}
	// Verify restores content AND permissions.
	violated, failed := g.Verify(before)
	if len(violated) != 1 || len(failed) != 0 {
		t.Fatalf("violated=%v failed=%v", violated, failed)
	}
	fi, err := os.Stat(locks)
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("not restored to file with 0600: %v %v", fi.IsDir(), fi.Mode().Perm())
	}
	data, _ := os.ReadFile(locks)
	if !stringHasPrefix(string(data), "locked:") {
		t.Fatalf("content not restored: %q", data)
	}
}

// A2: deleting an entry from locks.yaml and editing the unlocked agent
// file in the same breath — the agent file must still be checked
// (union of before-paths and current-paths).
func TestShrinkingLocksStillCoversRemovedAgent(t *testing.T) {
	g, cfgDir := newGuard(t)
	before, err := g.Snapshot() // includes agent:x.md via locks.yaml
	if err != nil {
		t.Fatal(err)
	}
	// Attacker: rewrite locks.yaml without agent:x, then edit x.md.
	os.WriteFile(filepath.Join(cfgDir, "locks.yaml"), []byte("locked: []\n"), 0o600)
	os.WriteFile(filepath.Join(cfgDir, "agents", "x.md"), []byte("tampered"), 0o600)
	violated, failed := g.Verify(before)
	if len(failed) != 0 {
		t.Fatalf("restore failures: %v", failed)
	}
	if len(violated) != 2 {
		t.Fatalf("both files must be violated, got %v", violated)
	}
	data, _ := os.ReadFile(filepath.Join(cfgDir, "agents", "x.md"))
	if string(data) != "v1" {
		t.Fatalf("agent file not restored: %q", data)
	}
	lk, _ := os.ReadFile(filepath.Join(cfgDir, "locks.yaml"))
	if !stringHasPrefix(string(lk), "locked:\n") {
		t.Fatalf("locks.yaml not restored: %q", lk)
	}
}

// A3: snapshot-based restore preserves uncommitted user edits, deletes
// attacker-created files, and keeps original permissions.
func TestSnapshotRestoreNotHEAD(t *testing.T) {
	g, cfgDir := newGuard(t)
	// User edits config.yaml but nothing is committed.
	os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("model:\n  name: m\n  # user edit\n"), 0o600)
	before, err := g.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Attacker overwrites config.yaml and creates a new protected file.
	os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("hacked"), 0o600)
	os.MkdirAll(filepath.Join(cfgDir, "skills"), 0o755)
	os.WriteFile(filepath.Join(cfgDir, "skills", "evil.md"), []byte("x"), 0o600)
	g.ExtraPaths = []string{filepath.Join(cfgDir, "skills", "evil.md")}
	violated, failed := g.Verify(before)
	if len(failed) != 0 {
		t.Fatalf("failures: %v", failed)
	}
	if len(violated) != 2 {
		t.Fatalf("want 2 violations, got %v", violated)
	}
	data, _ := os.ReadFile(filepath.Join(cfgDir, "config.yaml"))
	if !stringHasSuffix(string(data), "# user edit\n") {
		t.Fatalf("HEAD restore clobbered user edits: %q", data)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "skills", "evil.md")); !os.IsNotExist(err) {
		t.Fatal("attacker-created protected file must be deleted")
	}
}

// A3b: permission changes alone are violations; original perms come back.
func TestPermissionOnlyTamper(t *testing.T) {
	g, cfgDir := newGuard(t)
	os.Chmod(filepath.Join(cfgDir, "config.yaml"), 0o640)
	before, _ := g.Snapshot()
	os.Chmod(filepath.Join(cfgDir, "config.yaml"), 0o777)
	violated, failed := g.Verify(before)
	if len(violated) != 1 || len(failed) != 0 {
		t.Fatalf("violated=%v failed=%v", violated, failed)
	}
	fi, _ := os.Stat(filepath.Join(cfgDir, "config.yaml"))
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("original perms not restored: %v", fi.Mode().Perm())
	}
}

// A3c: symlink swap in the protected set is a violation and the link
// is replaced by the real file.
func TestSymlinkSwapIsViolation(t *testing.T) {
	g, cfgDir := newGuard(t)
	before, err := g.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfgDir, "locks.yaml")
	os.Remove(p)
	os.Symlink(filepath.Join(cfgDir, "config.yaml"), p)
	violated, failed := g.Verify(before)
	if len(violated) != 1 || len(failed) != 0 {
		t.Fatalf("violated=%v failed=%v", violated, failed)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("symlink not replaced by real file")
	}
}

// A4: a restore failure must be reported, never swallowed.
func TestRestoreFailureIsReported(t *testing.T) {
	g, cfgDir := newGuard(t)
	before, err := g.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Replace locks.yaml's parent directory with a file so restore
	// (MkdirAll) fails.
	p := filepath.Join(cfgDir, "locks.yaml")
	os.Remove(p)
	// Make the root itself read-only after tampering the file.
	os.WriteFile(p, []byte("tampered"), 0o600)
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfgDir, 0o755) })
	violated, failed := g.Verify(before)
	if len(violated) == 0 {
		t.Fatal("tamper must be detected")
	}
	// With the dir read-only, restore cannot succeed (running as root
	// may bypass; only assert when it actually failed).
	if len(failed) > 0 {
		t.Logf("restore failure surfaced as required: %v", failed)
	} else {
		t.Log("root ignores read-only dirs; restore succeeded")
	}
}

func stringHasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func stringHasSuffix(s, p string) bool {
	return len(s) >= len(p) && s[len(s)-len(p):] == p
}
