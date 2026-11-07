package scripts_test

// Tests for scripts/secretscan (invoked as an external process).
// Fake keys are assembled at RUNTIME so the test source itself never
// contains a scan-triggering literal (the scanner reads patch text,
// and this file is committed).

import (
	"crypto/rand"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scannerRel = "scripts/secretscan"

func scannerPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", scannerRel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("scanner script missing: %v", err)
	}
	return p
}

// fakeSecret assembles a scan-triggering key at runtime (never a
// literal in this source).
func fakeSecret() string {
	return "sk-" + "aaaa" + "1111" + "bbbb" + "2222" + "cccc" + "3333" + "dddd"
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	return dir
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	c := exec.Command("git", "add", ".")
	c.Dir = dir
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	c = exec.Command("git", "commit", "-qm", "commit "+name)
	c.Dir = dir
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
}

func scan(t *testing.T, repo, rangeArg string) (string, int) {
	t.Helper()
	cmd := exec.Command(scannerPath(t), rangeArg)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("scanner run: %v", err)
	}
	return string(out), code
}

// TestSecretScanBlocksFakeKeyCommit: a commit containing a fake
// (non-TEST-marked) sk- key must be blocked.
func TestSecretScanBlocksFakeKeyCommit(t *testing.T) {
	dir := initRepo(t)
	commitFile(t, dir, "base.txt", "base\n")
	commitFile(t, dir, "clean.txt", "nothing to see\n")
	out, code := scan(t, dir, "HEAD~1..HEAD")
	if code != 0 || !strings.Contains(out, "clean") {
		t.Fatalf("clean commit must pass: exit=%d out=%s", code, out)
	}
	commitFile(t, dir, "leak.txt", "token = "+fakeSecret()+"\n")
	out, code = scan(t, dir, "HEAD~1..HEAD")
	if code == 0 {
		t.Fatalf("fake key commit must be blocked, out=%s", out)
	}
	if !strings.Contains(out, "rule 1: known token prefix") {
		t.Fatalf("expected rule 1 hit, out=%s", out)
	}
}

// TestSecretScanCleanTreePasses: normal source content must not trip
// entropy/prefix rules (module paths, commit SHAs, hex ids).
func TestSecretScanCleanTreePasses(t *testing.T) {
	dir := initRepo(t)
	commitFile(t, dir, "base.txt", "base\n")
	content := "module github.com/FuYue-WanJie/owltide/core/guard\n" +
		"sha 8f12af1df91e4cf7fd4004e15a071e25e95cf748 and short e57a1aa\n" +
		"spill id 0123456789abcdef\n"
	commitFile(t, dir, "normal.go", content)
	out, code := scan(t, dir, "HEAD~1..HEAD")
	if code != 0 {
		t.Fatalf("normal content must pass: exit=%d out=%s", code, out)
	}
}

// TestSecretScanBlocksHighEntropy: a runtime-built high-entropy base64
// string (no known prefix) must be caught by rule 3.
func TestSecretScanBlocksHighEntropy(t *testing.T) {
	dir := initRepo(t)
	commitFile(t, dir, "base.txt", "base\n")
	commitFile(t, dir, "clean.txt", "ok\n")
	// 40 chars from a 50-symbol alphabet, generated at runtime so the
	// source never contains a scan-triggering literal.
	// Split so no single literal reaches the entropy threshold.
	const alphabetA = "abcdefghjkmnpqrstuvwxyz"
	const alphabetB = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	alphabet := alphabetA + alphabetB
	var b []byte
	for i := 0; i < 40; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, alphabet[n.Int64()])
	}
	secret := string(b)
	commitFile(t, dir, "entropy.txt", "value = "+secret+"\n")
	out, code := scan(t, dir, "HEAD~1..HEAD")
	if code == 0 {
		t.Fatalf("high-entropy string must be blocked, out=%s", out)
	}
	if !strings.Contains(out, "rule 3: high-entropy") {
		t.Fatalf("expected rule 3 hit, out=%s", out)
	}
}

// TestSecretScanHashRulePinned: a pinned sha256 (scripts/secretscan.hashes)
// blocks the key even when it carries a fake TEST marker.
func TestSecretScanHashRulePinned(t *testing.T) {
	dir := initRepo(t)
	commitFile(t, dir, "base.txt", "base\n")
	commitFile(t, dir, "clean.txt", "ok\n")
	// The pinned revoked key, assembled at runtime.
	pinned := "sk-VK" + "ZGTLYmYqzhAI3" + "RojKsVgFKuyfs" + "RIyblLMHOvDXXXLwTYRB"
	commitFile(t, dir, "pinned.txt", "old = "+pinned+"\n")
	out, code := scan(t, dir, "HEAD~1..HEAD")
	if code == 0 {
		t.Fatalf("pinned revoked key must be blocked, out=%s", out)
	}
	if !strings.Contains(out, "rule 4") {
		t.Fatalf("expected rule 4 hit, out=%s", out)
	}
}

// TestPrePushHookEndToEnd: a bare origin + clone with hooksPath set;
// pushing a secret commit must fail, a clean push must succeed.
func TestPrePushHookEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	origin := t.TempDir()
	c := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v: %s", err, out)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	c = exec.Command("git", "clone", "-q", origin, clone)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	// Point the clone's hooks at this repo's scripts/hooks (absolute);
	// the hook resolves its sibling scanner from $0.
	c = exec.Command("git", "config", "core.hooksPath",
		filepath.Dir(scannerPath(t))+"/hooks")
	c.Dir = clone
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("hooksPath: %v: %s", err, out)
	}

	run := func(args ...string) (string, int) {
		cmd := exec.Command("git", args...)
		cmd.Dir = clone
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out), code
	}

	// Clean push succeeds.
	commitFile(t, clone, "a.txt", "fine\n")
	if out, code := run("push", "-q", "origin", "main"); code != 0 {
		t.Fatalf("clean push must succeed: %d %s", code, out)
	}

	// Secret push is refused by the hook.
	commitFile(t, clone, "b.txt", "x = "+fakeSecret()+"\n")
	out, code := run("push", "origin", "main")
	if code == 0 {
		t.Fatalf("secret push must be refused: %s", out)
	}
	if !strings.Contains(out, "push refused") {
		t.Fatalf("expected scanner refusal message, got: %s", out)
	}
	// The remote must not have the secret commit.
	c = exec.Command("git", "-C", origin, "log", "--oneline", "main")
	refout, _ := c.CombinedOutput()
	if strings.Contains(string(refout), "commit b.txt") {
		t.Fatal("secret commit reached the remote")
	}
}
