package main

// M15 tests: session-allow slash commands, exact-argv signatures, and
// the structured confirmer (session allow pre-approves non-risk
// commands; risk-set commands always prompt).

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FuYue-WanJie/owltide/core/tool"
	"github.com/FuYue-WanJie/owltide/internal/perm"
)

// captureStdout collects everything fn prints to os.Stdout.
func captureStdout(fn func()) string {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return ""
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data)
}

func TestAllowCommandText(t *testing.T) {
	svc, _ := newSvc(t, time.Second)
	if !strings.Contains(allowCommandText(svc, "/allow list"), "暂无授权命令") {
		t.Fatal("empty list message missing")
	}
}

func allowCommandText(svc *perm.Service, line string) string {
	// The chat loop prints handleAllowCommand's output; capture it.
	// Non-slash lines are not handled and produce no output.
	return captureStdout(func() { handleAllowCommand(svc, line) })
}

func TestAllowAddAndUse(t *testing.T) {
	svc, _ := newSvc(t, time.Second)
	allowCommandText(svc, "/allow add cp a.txt b.txt")
	sig := tool.CommandSignature("cp   a.txt b.txt") // whitespace-normalized argv
	if !svc.CommandAllowed(sig) {
		t.Fatal("signature must be allowlisted (argv normalization)")
	}
	if svc.CommandAllowed("cp a.txt c.txt") {
		t.Fatal("different command must not match")
	}

	// Structured confirmer: allowed non-risk command auto-approves
	// without prompting.
	prompted := false
	detail := confirmDetail(svc, "s1", func(string) bool {
		prompted = true
		return false
	})
	if !detail("DANGEROUS command: cp a.txt b.txt — allow?", "command", "cp a.txt b.txt") {
		t.Fatal("allowed non-risk command must pass")
	}
	if prompted {
		t.Fatal("allowed command must not prompt")
	}

	// Not-allowed command still prompts.
	if detail("DANGEROUS command: cp x y — allow?", "command", "cp x y") {
		t.Fatal("unallowed command must not auto-approve")
	}
}

func TestAllowRiskRefused(t *testing.T) {
	svc, _ := newSvc(t, time.Second)
	out := allowCommandText(svc, "/allow add rm -rf x")
	if !strings.Contains(out, "拒绝") || !strings.Contains(out, "风险") {
		t.Fatalf("risk command must be refused: %s", out)
	}
	if svc.CommandAllowed(tool.CommandSignature("rm -rf x")) {
		t.Fatal("risk command must not be allowlisted")
	}
	// Even if somehow allowed, the structured confirmer must prompt:
	prompted := false
	detail := confirmDetail(svc, "s1", func(string) bool {
		prompted = true
		return true
	})
	if !detail("DANGEROUS command: rm -rf x — allow?", "command", "rm -rf x") {
		t.Fatal("risk command confirmation flow must work")
	}
	if !prompted {
		t.Fatal("risk command must always prompt")
	}
}

func TestAllowRevoke(t *testing.T) {
	svc, _ := newSvc(t, time.Second)
	allowCommandText(svc, "/allow add cp a.txt b.txt")
	out := allowCommandText(svc, "/allow revoke cp a.txt b.txt")
	if !strings.Contains(out, "已撤销") {
		t.Fatalf("revoke message missing: %s", out)
	}
	if len(svc.Allows()) != 0 {
		t.Fatal("allowlist must be empty after revoke")
	}
	out = allowCommandText(svc, "/allow revoke cp a.txt b.txt")
	if !strings.Contains(out, "未找到") {
		t.Fatalf("revoke of missing entry: %s", out)
	}
	allowCommandText(svc, "/allow add cp a.txt b.txt")
	out = allowCommandText(svc, "/allow revoke-all")
	if !strings.Contains(out, "已清空") || len(svc.Allows()) != 0 {
		t.Fatalf("revoke-all failed: %s", out)
	}
	out = allowCommandText(svc, "/allow bogus")
	if !strings.Contains(out, "usage:") {
		t.Fatalf("bogus subcommand must print usage: %s", out)
	}
	out = allowCommandText(svc, "normal task text")
	if out != "" {
		t.Fatalf("non-slash text must pass through, got %q", out)
	}
}
