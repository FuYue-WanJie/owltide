package spill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/tool"
)

func newStore(t *testing.T, max int) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := New(root, "sess1", max)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

// Big outputs are truncated with a working spill id; small outputs
// pass through untouched.
func TestInlineVsSpill(t *testing.T) {
	s, _ := newStore(t, 2*1024)

	small := "tiny result"
	out, err := s.Result(small)
	if err != nil || out != small {
		t.Fatalf("small output must pass through: %q %v", out, err)
	}

	big := strings.Repeat("line\n", 2000) // 10KB
	out, err = s.Result(big)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "OUTPUT TRUNCATED") || !strings.Contains(out, "[spill id:") {
		t.Fatalf("truncation banner missing: %q", out[:200])
	}
	if strings.Contains(out, "[sha256:") {
		t.Fatal("unexpected content")
	}

	// Full content retrievable via the spill id.
	id := between(out, "[spill id: ", " —")
	total := len(strings.Split(big, "\n"))
	got, err := s.Read(id + ":1-" + itoa(total))
	if err != nil {
		t.Fatal(err)
	}
	if got != Scrub(big) {
		t.Fatalf("spill roundtrip mismatch: got %d bytes want %d", len(got), len(big))
	}

	// Line-range read.
	part, err := s.Read(id + ":1998-2000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(part, "line") || len(strings.Split(part, "\n")) != 3 {
		t.Fatalf("range read: %q", part)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], b)
	if j < 0 {
		return ""
	}
	return s[i+len(a) : i+j]
}

// Errors truncate but keep the tail; content is scrubbed of secrets.
func TestErrorTailAndScrub(t *testing.T) {
	s, _ := newStore(t, 512)

	noisy := strings.Repeat("progress...\n", 200) +
		"ERROR: build failed at line 900\nkey=sk-TESTKEY00000000000000000000000000000000000000\n"
	out, err := s.Result("ERROR: " + noisy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "build failed") {
		t.Fatal("error tail must be kept in the excerpt")
	}
	id := between(out, "[spill id: ", " —")
	lines := strings.Count(Scrub(noisy), "\n") + 1
	full, _ := s.Read(id + ":1-" + itoa(lines))
	if strings.Contains(full, "sk-TESTKEY000") {
		t.Fatal("API key leaked into spill file")
	}
	if !strings.Contains(full, "sk-T") || !strings.Contains(full, "0000") || strings.Contains(full, "sk-TESTKEY00000000") {
		t.Fatalf("scrub did not run on spill content: %q", full[maxInt(0, len(full)-120):])
	}
	// Also: the excerpt in context must not carry the raw key.
	if strings.Contains(out, "sk-TESTKEY000") {
		t.Fatal("key leaked into context excerpt")
	}
}

// Spill dirs of other sessions are cleaned; the current one stays.
func TestCleanupSession(t *testing.T) {
	root := t.TempDir()
	old, err := New(root, "old-sess", 0)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(old.Dir, "deadbeefdeadbeef"), []byte("x"), 0o600)
	cur, err := New(root, "cur-sess", 0)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cur.Dir, "0123456789abcdef"), []byte("y"), 0o600)

	if err := CleanupSession(root, "cur-sess"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old.Dir); !os.IsNotExist(err) {
		t.Fatal("old session spill not removed")
	}
	if _, err := os.Stat(filepath.Join(cur.Dir, "0123456789abcdef")); err != nil {
		t.Fatal("current session spill removed")
	}
}

// read_file tool integration: spill:<id> serves the stored output.
func TestReadFileSpillRef(t *testing.T) {
	s, _ := newStore(t, 256)
	big := strings.Repeat("data ", 500)
	out, err := s.Result(big)
	if err != nil {
		t.Fatal(err)
	}
	id := between(out, "[spill id: ", " —")

	tool.SetSpillReader(func(ref string) (string, error) { return s.Read(ref) })
	t.Cleanup(func() { tool.SetSpillReader(nil) })
	rf := tool.ReadFile{}
	got, err := rf.Call(t.Context(), []byte(`{"path":"spill:`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "data") {
		t.Fatalf("spill read: %q", got[:80])
	}
	// Invalid ids are rejected.
	if _, err := rf.Call(t.Context(), []byte(`{"path":"spill:../etc/passwdxxxx"}`)); err == nil {
		t.Fatal("invalid spill id must be rejected")
	}
}
