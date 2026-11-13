package decision

// AppendEvent writes one JSON line per call to decisions.jsonl with
// the event name and fields verbatim (audit-safe values only).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendEvent(t *testing.T) {
	dir := t.TempDir()
	if err := AppendEvent(dir, "permission", map[string]any{
		"id": "req-1", "kind": "approval", "state": "approved", "by": "cli", "source": "main",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["event"] != "permission" || rec["id"] != "req-1" || rec["state"] != "approved" {
		t.Fatalf("record = %v", rec)
	}
	if _, ok := rec["ts"]; !ok {
		t.Fatal("record missing ts")
	}
}
