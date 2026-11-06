package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func TestDangerousShellNeedsConfirmation(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	sh := NewShell("/bin/sh", "-c", dir, 5)

	// No confirmer wired: dangerous command rejected, nothing runs.
	_, err := sh.Call(context.Background(), rawJSON(map[string]string{"command": "rm -rf " + dir + "/sub"}))
	if err == nil || !strings.Contains(err.Error(), "user confirmation") {
		t.Fatalf("want confirmation rejection, got %v", err)
	}
	// Harmless command still runs without confirmer.
	out, err := sh.Call(context.Background(), rawJSON(map[string]string{"command": "echo fine"}))
	if err != nil || out != "fine\n" {
		t.Fatalf("harmless: %q %v", out, err)
	}

	// Confirmer says no: rejected.
	sh.setConfirmer(func(string) bool { return false })
	if _, err := sh.Call(context.Background(), rawJSON(map[string]string{"command": "rm -rf " + dir + "/sub"})); err == nil {
		t.Fatal("confirmer=false must reject")
	}

	// Confirmer says yes: executes. (rm -rf on an empty sub-path is safe.)
	sh.setConfirmer(func(string) bool { return true })
	out, err = sh.Call(context.Background(), rawJSON(map[string]string{"command": "rm -rf " + dir + "/nonexistent-sub"}))
	if err != nil {
		t.Fatalf("confirmed run: %v", err)
	}
}

func TestDangerPatterns(t *testing.T) {
	dangerous := []string{
		"rm -rf /tmp/x",
		"rm -fr /tmp/x",
		"cd /tmp && rm -r -f x",
		"git push --force origin main",
		"git push -f origin main",
		"git reset --hard HEAD~1",
		"dd if=/dev/zero of=/dev/sda",
		"chmod -R 777 /",
		"shutdown now",
	}
	for _, c := range dangerous {
		if !isDangerousCommand(c) {
			t.Fatalf("%q must be dangerous", c)
		}
	}
	safe := []string{"echo ok", "git push origin main", "git status", "rm file.txt", "git commit -am 'force the issue'"}
	for _, c := range safe {
		if isDangerousCommand(c) {
			t.Fatalf("%q must be safe", c)
		}
	}
}

func TestLocksFileWriteNeedsConfirmation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "locks.yaml")
	wf := &WriteFile{}
	// No confirmer: rejected.
	_, err := wf.Call(context.Background(), rawJSON(map[string]string{"path": p, "content": "locked: []\n"}))
	if err == nil || !strings.Contains(err.Error(), "user confirmation") {
		t.Fatalf("want lock-write rejection, got %v", err)
	}
	// Regular file unaffected.
	okPath := filepath.Join(dir, "note.txt")
	if _, err := wf.Call(context.Background(), rawJSON(map[string]string{"path": okPath, "content": "hi"})); err != nil {
		t.Fatal(err)
	}
	// Confirmed: allowed.
	wf.setConfirmer(func(string) bool { return true })
	if _, err := wf.Call(context.Background(), rawJSON(map[string]string{"path": p, "content": "locked: []\n"})); err != nil {
		t.Fatalf("confirmed lock write: %v", err)
	}
	// edit_file path is gated too.
	ef := &EditFile{}
	if _, err := ef.Call(context.Background(), rawJSON(map[string]any{
		"path": p, "old_string": "locked", "new_string": "x",
		"expect_sha256": sha256Hex("locked: []\n"),
	})); err == nil || !strings.Contains(err.Error(), "user confirmation") {
		t.Fatalf("edit_file on locks.yaml must be gated, got %v", err)
	}
}
