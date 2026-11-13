package tool

// M15 tests: the non-overridable risk set and the exact-argv command
// signature used by session allows.

import (
	"strings"
	"testing"
)

func TestIsRiskCommand(t *testing.T) {
	risky := []string{
		"rm -rf x",
		"rm x",
		"git push --force origin main",
		"git clean -fdx",
		"find . -name '*.tmp' -delete",
		"sudo ls",
		"dd if=/dev/zero of=/dev/sda",
		"chmod 777 x",
		"echo hi > locks.yaml",
		"shutdown -h now",
		"$(date)",
	}
	for _, cmd := range risky {
		if !IsRiskCommand(cmd) {
			t.Errorf("%q must be in the risk set", cmd)
		}
	}
	safe := []string{
		"echo hi",
		"git status",
		"git push origin main",
		"ls -la",
		"go test ./...",
		"cat a.txt",
	}
	for _, cmd := range safe {
		if IsRiskCommand(cmd) {
			t.Errorf("%q must NOT be in the risk set", cmd)
		}
	}
}

func TestCommandSignature(t *testing.T) {
	a := CommandSignature("cp   a.txt   b.txt")
	b := CommandSignature("cp a.txt b.txt")
	if a != b {
		t.Fatalf("whitespace must normalize: %q vs %q", a, b)
	}
	if CommandSignature("cp a.txt c.txt") == a {
		t.Fatal("different argv must not share a signature")
	}
	// Dynamic commands fall back to raw text (never allowlisted).
	dyn := "$(echo rm) x"
	if got := CommandSignature(dyn); got != dyn {
		t.Fatalf("dynamic signature = %q, want raw text %q", got, dyn)
	}
	// Multi-command lines keep both call signatures.
	sig := CommandSignature("echo a && echo b")
	if !strings.Contains(sig, "echo\x00a") || !strings.Contains(sig, "echo\x00b") {
		t.Fatalf("multi-call signature = %q", sig)
	}
}
