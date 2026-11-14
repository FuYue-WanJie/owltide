package agent

// P2b-M1 tests: legacy todo.md upgrade keeps the body; frontmatter is
// added once.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLegacyTodoUpgradedBodyKept: an old frontmatter-less todo.md is
// upgraded on the first write with its content preserved verbatim.
func TestLegacyTodoUpgradedBodyKept(t *testing.T) {
	dir := t.TempDir()
	legacy := "# Goal\n- [ ] ship it\n- [x] plan it"
	if err := os.WriteFile(filepath.Join(dir, "todo.md"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	// Upgrade first (no Call): the legacy body is preserved verbatim.
	up := NewTodoUpgrade()
	if err := upgradeTodo(dir, "s-1", up); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "todo.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\nkind: todo\n") {
		t.Fatalf("frontmatter missing:\n%s", text)
	}
	if !strings.Contains(text, "id: t-") || !strings.Contains(text, "session: s-1") {
		t.Fatalf("frontmatter fields missing:\n%s", text)
	}
	if !strings.Contains(text, legacy) {
		t.Fatalf("legacy body lost:\n%s", text)
	}

	// A subsequent tool write replaces the body and keeps the
	// frontmatter (single document, no duplication).
	td := TodoTool{Dir: dir, Session: "s-1", Upgrade: up}
	if _, err := td.Call(context.Background(), json.RawMessage(`{"content":"- [x] ship it\n- [x] more"}`)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "todo.md"))
	text = string(data)
	if strings.Count(text, "---") != 2 {
		t.Fatalf("frontmatter duplicated:\n%s", text)
	}
	if strings.Contains(text, legacy) {
		t.Fatalf("old body should be replaced by the full-content write:\n%s", text)
	}
	if !strings.Contains(text, "- [x] more") {
		t.Fatalf("new body missing:\n%s", text)
	}
}

// TestFreshTodoGetsFrontmatter: a brand-new session's first todo write
// carries the frontmatter immediately.
func TestFreshTodoGetsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	td := TodoTool{Dir: dir, Session: "s-2", Upgrade: NewTodoUpgrade()}
	if _, err := td.Call(context.Background(), json.RawMessage(`{"content":"- [ ] one"}`)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "todo.md"))
	if !strings.HasPrefix(string(data), "---\nkind: todo\n") {
		t.Fatalf("fresh write missing frontmatter:\n%s", string(data))
	}
}
