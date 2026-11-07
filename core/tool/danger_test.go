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
		"rm --recursive --force /tmp/x",
		"cd /tmp && rm -r -f x",
		"rm file.txt && rm -rf dir",
		"find /tmp -name '*.log' -delete",
		"find . -type f -exec rm {} ;",
		"git push --force origin main",
		"git push -f origin main",
		"git push --force-with-lease origin main",
		"git push origin +main",
		"git reset --hard HEAD~1",
		"git clean -fdx",
		"echo prelude && rm -rf /tmp/x",
		"echo a; rm -rf /tmp/x",
		"echo a | xargs rm -rf /tmp/x",
		"sh -c 'rm -rf /tmp/x'",
		"bash -c \"git push --force origin main\"",
		"eval 'rm -rf /tmp/x'",
		"sudo rm -rf /tmp/x",
		"dd if=/dev/zero of=/dev/sda",
		"chmod -R 777 /",
		"shutdown now",
		// unparseable garbage is dangerous by default
		"rm -rf $(echo /tmp/x' ",
		// B1: quoted command names must not bypass (static word eval)
		"'rm' -rf /tmp/x",
		"rm \"-rf\" /tmp/x",
		"\\rm -rf /tmp/x",
		"r''m -rf /tmp/x",
		// B1: non-static words are dangerous outright
		"$X -rf /tmp",
		"rm $(find /) ",
		"rm -rf $TARGET",
		// B2: sudo edge cases
		"sudo -u root rm -rf /",
		"sudo -E rm -rf /tmp/x",
		"sudo -- rm -rf /tmp/x",
		// B3: wrapper binaries
		"env rm -rf /tmp/x",
		"env VAR=1 -u HOME rm -rf /tmp/x",
		"nohup rm -rf /tmp/x",
		"timeout 10 rm -rf /tmp/x",
		"nice -n 5 rm -rf /tmp/x",
		"ionice rm -rf /tmp/x",
		"stdbuf -oL rm -rf /tmp/x",
		"time rm -rf /tmp/x",
		"command rm -rf /tmp/x",
		"exec rm -rf /tmp/x",
		"busybox rm -rf /tmp/x",
		"toybox rm -rf /tmp/x",
		// B4: git global options before the subcommand
		"git -C /repo push --force origin main",
		"git -c x=y reset --hard HEAD~1",
		"git --git-dir=/r push -f origin main",
		"git -C /repo clean -fdx",
		// A4: delayed write via nohup
		"nohup sh -c 'sleep 5; echo x > locks.yaml' &",
		// risky command + dynamic args stays dangerous
		"rm -rf $TARGET",
		"rm $(find /)",
		"find $DIR -delete",
		"git push $REMOTE --force",
		// dynamic redirect target is dangerous
		"echo x > $F",
	}
	for _, c := range dangerous {
		if !isDangerousCommand(c) {
			t.Fatalf("%q must be dangerous", c)
		}
	}
	safe := []string{
		"echo ok",
		"git push origin main",
		"git status",
		"rm file.txt",
		"git commit -am 'force the issue'",
		"go test ./...",
		"git diff HEAD",
		// dynamic args on non-risky commands are fine
		"go test $(go list ./...)",
		"cd \"$HOME\"",
		"echo $PATH",
		"ls $DIR",
		"cat ${CONFIG_FILE}",
		"echo $(date)",
	}
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
