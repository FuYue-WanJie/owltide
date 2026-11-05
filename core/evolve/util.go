package evolve

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func readFileIfExists(p string) ([]byte, error)  { return os.ReadFile(p) }
func filepathRel(root, p string) (string, error) { return filepath.Rel(root, p) }
func joinPath(root string, parts ...string) string {
	return filepath.Join(append([]string{root}, parts...)...)
}
func mkdirParent(p string)                    { os.MkdirAll(filepath.Dir(p), 0o755) }
func osWriteFile(p string, data []byte) error { return os.WriteFile(p, data, 0o600) }

func parseWhen(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// tolerate truncated stamps
		if len(s) >= 16 {
			t, err = time.Parse("2006-01-02T15:04", s[:16])
		}
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

var _ = strings.TrimSpace
