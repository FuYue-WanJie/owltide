package sessionlog

import (
	"os"
	"path/filepath"
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
