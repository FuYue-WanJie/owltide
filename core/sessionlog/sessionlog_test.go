package sessionlog

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/model"
)

// TestSessionLogAppendRebuild: append a sequence, rebuild equals it.
func TestSessionLogAppendRebuild(t *testing.T) {
	root := t.TempDir()
	l, err := Open(root, "s1")
	if err != nil {
		t.Fatal(err)
	}
	in := []model.Message{
		{Role: model.RoleSystem, Content: "sys"},
		{Role: model.RoleUser, Content: "hi"},
		{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{
			ID: "c1", Type: "function",
			Function: model.Function{Name: "agent_result", Arguments: `{"id":"sub-x"}`},
		}}},
		{Role: model.RoleTool, ToolCallID: "c1", Name: "agent_result", Content: "data marker"},
	}
	for _, m := range in {
		if err := l.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	out, warn, err := RebuildWithWarning(filepath.Join(root, "s1", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if warn != "" {
		t.Fatalf("unexpected warning: %s", warn)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d messages, want %d", len(out), len(in))
	}
	for i := range in {
		if len(out[i].ToolCalls) != len(in[i].ToolCalls) ||
			out[i].Role != in[i].Role || out[i].Content != in[i].Content ||
			out[i].ToolCallID != in[i].ToolCallID || out[i].Name != in[i].Name {
			t.Fatalf("message %d mismatch: %+v != %+v", i, out[i], in[i])
		}
		for j := range in[i].ToolCalls {
			if out[i].ToolCalls[j] != in[i].ToolCalls[j] {
				t.Fatalf("tool call %d/%d mismatch", i, j)
			}
		}
	}
}

// TestSessionLogTruncatedTail: a torn final line is dropped with a
// warning and the rest rebuilds.
func TestSessionLogTruncatedTail(t *testing.T) {
	root := t.TempDir()
	l, err := Open(root, "s2")
	if err != nil {
		t.Fatal(err)
	}
	good1 := model.Message{Role: model.RoleUser, Content: "first"}
	good2 := model.Message{Role: model.RoleAssistant, Content: "second"}
	for _, m := range []model.Message{good1, good2} {
		if err := l.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a torn write: close, then append garbage bytes without
	// a trailing newline (crash residue).
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "s2", "messages.jsonl")
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(`{"role":"user","cont`); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	out, warn, err := RebuildWithWarning(p)
	if err != nil {
		t.Fatalf("truncated tail must not error: %v", err)
	}
	if warn == "" || !strings.Contains(warn, "truncated final line") {
		t.Fatalf("expected truncation warning, got %q", warn)
	}
	if len(out) != 2 || out[0].Content != "first" || out[1].Content != "second" {
		t.Fatalf("rebuild mismatch: %+v", out)
	}
}

// TestResumeAfterTruncatedTailAppendsCleanly: opening a session with a
// torn tail truncates to the last clean record; the next appends then
// rebuild without losing any good record.
func TestResumeAfterTruncatedTailAppendsCleanly(t *testing.T) {
	root := t.TempDir()
	l, err := Open(root, "s3")
	if err != nil {
		t.Fatal(err)
	}
	good := []model.Message{
		{Role: model.RoleUser, Content: "first"},
		{Role: model.RoleAssistant, Content: "second"},
	}
	for _, m := range good {
		if err := l.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Torn tail.
	p := filepath.Join(root, "s3", "messages.jsonl")
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(`{"role":"user","cont`); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	// Resume: torn tail truncated, clean messages returned.
	msgs, warn, err := RepairTornTail(p)
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("expected truncation warning on resume")
	}
	if len(msgs) != 2 || msgs[0].Content != "first" || msgs[1].Content != "second" {
		t.Fatalf("resume rebuild mismatch: %+v", msgs)
	}
	// Open for continued appending after the truncation.
	l2, err := Open(root, "s3")
	if err != nil {
		t.Fatal(err)
	}
	// Append a good record after the truncation.
	if err := l2.Append(model.Message{Role: model.RoleUser, Content: "third"}); err != nil {
		t.Fatal(err)
	}
	l2.Close()

	// Rebuild: all three good records, no loss, no warning.
	again, warn2, err := RebuildWithWarning(p)
	if err != nil {
		t.Fatal(err)
	}
	if warn2 != "" {
		t.Fatalf("post-truncation rebuild must be clean, got warning: %s", warn2)
	}
	if len(again) != 3 || again[2].Content != "third" {
		t.Fatalf("records lost or misordered after resume: %+v", again)
	}
}

// TestResumeRejectsConcurrentOpen: a live lock refuses a second open;
// a lock whose holder died is reported stale and taken over; release
// only removes our own lock.
func TestResumeRejectsConcurrentOpen(t *testing.T) {
	root := t.TempDir()

	lock, stale, err := LockSession(root, "s-live")
	if err != nil || stale {
		t.Fatalf("fresh lock: lock=%v stale=%v err=%v", lock, stale, err)
	}
	// Second open while we are alive: refused with our pid.
	_, _, err = LockSession(root, "s-live")
	if err == nil || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("concurrent open must be refused, got: %v", err)
	}
	// Release works for the holder.
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	// Release is idempotent.
	if err := lock.Release(); err != nil {
		t.Fatalf("release must be idempotent: %v", err)
	}

	// Stale lock: record the pid of a dead process.
	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Skip("cannot start a process for the stale test")
	}
	deadPid := dead.Process.Pid
	_ = dead.Wait() // it is gone now

	lockPath := filepath.Join(root, "s-stale", ".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(deadPid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, stale, err := LockSession(root, "s-stale")
	if err != nil {
		t.Fatalf("stale lock must be taken over: %v", err)
	}
	if !stale {
		t.Fatal("takeover must be reported as stale")
	}
	// The lock file now records OUR pid.
	data, _ := os.ReadFile(lockPath)
	if !strings.Contains(string(data), strconv.Itoa(os.Getpid())) {
		t.Fatalf("lock file must record the new holder: %q", data)
	}
	if err := got.Release(); err != nil {
		t.Fatal(err)
	}

	// A lock held by a LIVE foreign pid (our own pid here) is refused
	// and never removed by Release of another lock.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := &SessionLock{path: lockPath, pid: 999999}
	if err := other.Release(); err == nil {
		t.Fatal("a lock must not be released by a non-holder")
	}
	if _, _, err := LockSession(root, "s-stale"); err == nil {
		t.Fatal("live pid lock must refuse")
	}
}
