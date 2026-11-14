package agent

// Todo document upgrade (P2b-M1): todo.md gains the standard
// frontmatter (kind/session/created/updated; status mirrors the
// checkboxes). Existing frontmatter-less files are upgraded in place
// on the first write — the old body is preserved verbatim.
// Upgrade state lives on the tool instance (one per session).

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TodoUpgrade tracks whether the legacy file was already migrated
// (exported: cross-package fixtures wire it into TodoTool).
type TodoUpgrade struct {
	done bool
}

// NewTodoUpgrade builds the session upgrade tracker.
func NewTodoUpgrade() *TodoUpgrade { return &TodoUpgrade{} }

// upgradeTodo ensures todo.md carries the frontmatter. The body is
// never altered: an old file's content becomes the body verbatim.
func upgradeTodo(dir, sessionID string, up *TodoUpgrade) error {
	if up.done {
		return nil
	}
	path := filepath.Join(dir, "todo.md")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		up.done = true // nothing to upgrade; first write creates fresh
		return nil
	}
	if err != nil {
		return err
	}
	text := string(data)
	if strings.HasPrefix(strings.TrimSpace(text), "---") {
		up.done = true // already has frontmatter
		return nil
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id := "t-" + hex.EncodeToString(b)
	now := time.Now().UTC().Format(time.RFC3339)
	fm := fmt.Sprintf("---\nkind: todo\nid: %s\nsession: %s\nstatus: active\ncreated: %s\nupdated: %s\n---\n",
		id, sessionID, now, now)
	if err := os.WriteFile(path, []byte(fm+text), 0o600); err != nil {
		return err
	}
	up.done = true
	return nil
}

// renderDoc produces the full todo.md: frontmatter + the model's body.
// The body replaces the old one (the tool contract is "full content
// each time"); the upgrade preserved the legacy body into the current
// document on the first call, and later bodies are the model's latest.
func (t TodoTool) renderDoc(body string) string {
	id := "t-new"
	session := t.Session
	now := time.Now().UTC().Format(time.RFC3339)
	status := "active"
	if !strings.Contains(body, "- [ ]") && strings.TrimSpace(body) != "" {
		status = "done"
	}
	// Reuse the existing id/session when the document already exists.
	if data, err := os.ReadFile(filepath.Join(t.Dir, "todo.md")); err == nil {
		text := string(data)
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "id: ") {
				id = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			}
			if strings.HasPrefix(line, "session: ") {
				session = strings.TrimSpace(strings.TrimPrefix(line, "session: "))
			}
			if strings.HasPrefix(line, "created: ") {
				now = strings.TrimSpace(strings.TrimPrefix(line, "created: "))
			}
		}
	}
	return fmt.Sprintf("---\nkind: todo\nid: %s\nsession: %s\nstatus: %s\ncreated: %s\nupdated: %s\n---\n%s\n",
		id, session, status, now, time.Now().UTC().Format(time.RFC3339), strings.TrimRight(body, "\n"))
}

// todoWiring returns the session-scoped upgrade hook used by main.
func todoWiring(dir, sessionID string) (*TodoUpgrade, func() error) {
	up := NewTodoUpgrade()
	return up, func() error { return upgradeTodo(dir, sessionID, up) }
}
