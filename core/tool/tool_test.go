package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadEditList(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	raw := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}

	wf := WriteFile{}
	out, err := wf.Call(ctx, raw(writeFileArgs{Content: "hello\nworld", fileArgs: fileArgs{Path: filepath.Join(dir, "a/b.txt")}}))
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("empty result")
	}

	rf := ReadFile{}
	got, err := rf.Call(ctx, raw(fileArgs{Path: filepath.Join(dir, "a/b.txt")}))
	if err != nil || got != "hello\nworld" {
		t.Fatalf("read: %q %v", got, err)
	}

	ef := EditFile{}
	if _, err := ef.Call(ctx, raw(editFileArgs{Old: "hello", New: "hi", fileArgs: fileArgs{Path: filepath.Join(dir, "a/b.txt")}})); err != nil {
		t.Fatal(err)
	}
	got, _ = rf.Call(ctx, raw(fileArgs{Path: filepath.Join(dir, "a/b.txt")}))
	if got != "hi\nworld" {
		t.Fatalf("edit result: %q", got)
	}

	// non-unique old_string must fail ("o" appears in both "world" and "hi\nworld"? no:
	// content is "hi\nworld"; "o" appears once). Use a string that repeats.
	if err := os.WriteFile(filepath.Join(dir, "a/b.txt"), []byte("aXaXa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ef.Call(ctx, raw(editFileArgs{Old: "a", New: "x", fileArgs: fileArgs{Path: filepath.Join(dir, "a/b.txt")}})); err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}
	// missing old_string must fail
	if _, err := ef.Call(ctx, raw(editFileArgs{Old: "zzz", New: "x", fileArgs: fileArgs{Path: filepath.Join(dir, "a/b.txt")}})); err == nil {
		t.Fatal("expected missing edit to fail")
	}

	ld := ListDir{}
	names, err := ld.Call(ctx, raw(fileArgs{Path: dir}))
	if err != nil || !contains(names, "a/") {
		t.Fatalf("list_dir: %q %v", names, err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestShellTimeoutAndOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	sh := NewShell("/bin/sh", "-c", t.TempDir(), 1)
	ctx := context.Background()

	out, err := sh.Call(ctx, json.RawMessage(`{"command":"echo ok"}`))
	if err != nil || out != "ok\n" {
		t.Fatalf("echo: %q %v", out, err)
	}

	out, err = sh.Call(ctx, json.RawMessage(`{"command":"sleep 5"}`))
	if err == nil {
		t.Fatal("expected timeout error")
	}

	out, err = sh.Call(ctx, json.RawMessage(`{"command":"echo err >&2; exit 3"}`))
	if err == nil || out != "err\n" {
		t.Fatalf("exit code case: %q %v", out, err)
	}
}
