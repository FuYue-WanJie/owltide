package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAfterExternalGc(t *testing.T) {
	s := newTestStore(t)
	write(t, s.Dir, "config.yaml", "v: 1\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "user", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "agents/a.yaml", "name: a\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "agent", Reason: "second"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "gc")
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), "GIT_DIR="+s.GitDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git gc: %v: %s", err, out)
	}
	log, err := s.Log()
	if err != nil {
		t.Fatalf("Log after gc: %v", err)
	}
	if len(log) != 2 {
		t.Fatalf("want 2 commits after gc, got %d", len(log))
	}
	// 引擎写第二个提交也必须能在 pack 存在时工作（新 loose 对象与 pack 共存）
	write(t, s.Dir, "agents/b.yaml", "name: b\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "agent", Reason: "third"}); err != nil {
		t.Fatalf("commit after gc: %v", err)
	}
}

func TestFullGitCompatCycle(t *testing.T) {
	s := newTestStore(t)
	write(t, s.Dir, "config.yaml", "v: 1\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "user", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	write(t, s.Dir, "agents/a.yaml", "name: a\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "agent", Reason: "second"}); err != nil {
		t.Fatal(err)
	}

	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = s.Dir
		cmd.Env = append(os.Environ(), "GIT_DIR="+s.GitDir, "GIT_WORK_TREE="+s.Dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}

	if out := git("fsck"); strings.Contains(out, "error") || strings.Contains(out, "missing") {
		t.Fatalf("fsck reported problems: %s", out)
	}
	git("gc")

	// Engine reads history from packs.
	log, err := s.Log()
	if err != nil || len(log) != 2 {
		t.Fatalf("engine log after gc: n=%d err=%v", len(log), err)
	}
	// Engine continues committing on top of packed history.
	write(t, s.Dir, "agents/b.yaml", "name: b\n")
	if _, _, err := s.AutoCommit(CommitMeta{Who: "agent", Reason: "third"}); err != nil {
		t.Fatalf("commit after gc: %v", err)
	}
	// Engine rolls back after gc.
	if _, err := s.RevertLast("cycle-test"); err != nil {
		t.Fatalf("revert after gc: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "agents/b.yaml")); !os.IsNotExist(err) {
		t.Fatal("revert after gc did not remove file")
	}
	// Real git still accepts the repo end-to-end.
	if out := git("fsck"); strings.Contains(out, "error") || strings.Contains(out, "missing") {
		t.Fatalf("fsck after engine writes: %s", out)
	}
	git("log", "--oneline") // must parse
	// Repeated gc cycles.
	git("gc")
	if _, err := s.Log(); err != nil {
		t.Fatalf("log after second gc: %v", err)
	}
}
