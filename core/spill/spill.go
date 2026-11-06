// Package spill moves oversized tool results out of the model context:
// the context keeps a head+tail excerpt with total sizes and a spill id
// that read_file can serve back with line ranges. Spill files live in
// ~/.owltide/spill/<session>/, are never committed to the config
// store, and are scrubbed for API-key-looking secrets before writing.
package spill

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultMaxBytes is the default per-result cap.
const DefaultMaxBytes = 8 * 1024

// MinimalMaxBytes is the cap for resource-constrained targets
// (Android etc.).
const MinimalMaxBytes = 2 * 1024

// HeadLines / TailLines bound the excerpt kept in context.
const HeadLines = 40
const TailLines = 10

// keyPatterns scrub obvious API keys before persisting spill files.
var keyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._-]{20,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`xox[bpars]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token)["':= ]+["']?[A-Za-z0-9._-]{24,}`),
}

// Scrub masks secret-looking substrings.
func Scrub(s string) string {
	for _, re := range keyPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			if len(m) <= 8 {
				return strings.Repeat("*", len(m))
			}
			return m[:4] + strings.Repeat("*", len(m)-8) + m[len(m)-4:]
		})
	}
	return s
}

// Store manages one session's spill directory.
type Store struct {
	Dir      string // ~/.owltide/spill/<session>
	MaxBytes int
}

// New creates the session spill dir. The spill dir is excluded from
// config-store commits by naming convention (store skips "spill").
func New(root, session string, maxBytes int) (*Store, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	dir := filepath.Join(root, "spill", session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, MaxBytes: maxBytes}, nil
}

// CleanupSession removes every spill dir except the current session's.
func CleanupSession(root, session string) error {
	base := filepath.Join(root, "spill")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != session {
			os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
	return nil
}

// EnforceSize caps the spill dir: oldest files go first when over
// budget. MaxSpillBytes bounds total size.
const MaxSpillBytes = 32 * 1024 * 1024

func (s *Store) enforceBudget() {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	type f struct {
		name string
		size int64
		mod  int64
	}
	var files []f
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			files = append(files, f{e.Name(), fi.Size(), fi.ModTime().UnixNano()})
			total += fi.Size()
		}
	}
	if total <= MaxSpillBytes {
		return
	}
	// oldest first
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j].mod < files[j-1].mod; j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
	for _, fl := range files {
		if total <= MaxSpillBytes {
			break
		}
		os.Remove(filepath.Join(s.Dir, fl.name))
		total -= fl.size
	}
}

// Result decides inline vs spill for one tool output and returns the
// text that goes into the model context.
func (s *Store) Result(output string) (string, error) {
	if len(output) <= s.MaxBytes {
		return output, nil
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(s.Dir, id), []byte(Scrub(output)), 0o600); err != nil {
		return "", err
	}
	s.enforceBudget()

	lines := strings.Split(output, "\n")
	total := len(lines)
	head := lines
	if len(head) > HeadLines {
		head = head[:HeadLines]
	}
	var tail []string
	if total > HeadLines+TailLines {
		tail = lines[total-TailLines:]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[OUTPUT TRUNCATED: %d bytes, %d lines; showing first %d lines%s]\n",
		len(output), total, len(head), tailNote(len(tail)))
	b.WriteString(Scrub(strings.Join(head, "\n")))
	if len(tail) > 0 {
		b.WriteString("\n... [" + fmt.Sprint(total-len(head)-len(tail)) + " lines omitted] ...\n")
		b.WriteString(Scrub(strings.Join(tail, "\n")))
	}
	fmt.Fprintf(&b, "\n\n[spill id: %s — full output saved. Call read_file with path \"spill:%s\" to fetch it, optionally with head/last line ranges like \"spill:%s:100-200\".]",
		id, id, id)
	return b.String(), nil
}

func tailNote(n int) string {
	if n > 0 {
		return fmt.Sprintf(" and last %d lines", n)
	}
	return ""
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Read serves a spill id (optionally "id:start-end" 1-based inclusive)
// back to the model.
func (s *Store) Read(ref string) (string, error) {
	id := ref
	var lo, hi int
	if i := strings.Index(ref, ":"); i >= 0 {
		id = ref[:i]
		rangePart := ref[i+1:]
		if _, err := fmt.Sscanf(rangePart, "%d-%d", &lo, &hi); err != nil {
			lo, hi = 0, 0
		}
	}
	if !validID(id) {
		return "", fmt.Errorf("invalid spill id")
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, id))
	if err != nil {
		return "", fmt.Errorf("spill %s not found (expired or from another session)", id)
	}
	lines := strings.Split(string(data), "\n")
	if lo > 0 || hi > 0 {
		if lo < 1 {
			lo = 1
		}
		if hi > len(lines) || hi == 0 {
			hi = len(lines)
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		lines = lines[lo-1 : hi]
	}
	out := strings.Join(lines, "\n")
	const maxRead = 64 * 1024
	if len(out) > maxRead {
		out = out[:maxRead] + "\n... (spill excerpt truncated; request a narrower line range)"
	}
	return out, nil
}

func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
		if !ok {
			return false
		}
	}
	return true
}
