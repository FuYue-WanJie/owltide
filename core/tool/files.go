package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sha256Hex hashes file content for anchored edits.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fragment returns a short view of the current content: context around
// the first occurrence of want when present, else the head of the file.
func fragment(content, want string) string {
	const maxFrag = 1500
	lines := strings.Split(content, "\n")
	if i := strings.Index(content, want); i >= 0 && want != "" {
		lineAt := strings.Count(content[:i], "\n")
		lo, hi := lineAt-5, lineAt+6
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		return clamp(strings.Join(lines[lo:hi], "\n"))
	}
	n := 25
	if n > len(lines) {
		n = len(lines)
	}
	return clamp(strings.Join(lines[:n], "\n"))
}

// maxOutput caps tool output fed back to the model.
const maxOutput = 20_000

func clamp(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + fmt.Sprintf("\n... (truncated, %d total bytes)", len(s))
}

// fileArgs covers path-style arguments.
type fileArgs struct {
	Path string `json:"path"`
}

func (a fileArgs) validate() error {
	if a.Path == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

// ---- read_file ----

type ReadFile struct{}

func (ReadFile) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name: "read_file",
			Description: "Read a text file. Returns the content followed by a final " +
				"\n[sha256:<hex>] marker line. Pass that hash as expect_sha256 when " +
				"calling edit_file on this path. Also serves truncated tool outputs: " +
				"path spill:<id> or spill:<id>:100-200 (1-based lines).",
			Parameters: object(`"path":{"type":"string"}`, "path"),
		},
	}
}

func (ReadFile) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a fileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	// spill:<id>[:start-end] serves saved oversized tool outputs.
	if strings.HasPrefix(a.Path, "spill:") {
		if spillReader == nil {
			return "", fmt.Errorf("no spill store configured")
		}
		return spillReader(a.Path[len("spill:"):])
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	return clamp(string(data)) + "\n[sha256:" + sha256Hex(string(data)) + "]", nil
}

// spillReader is wired by the agent setup to the session spill store.
var spillReader func(ref string) (string, error)

// SetSpillReader registers the spill lookup used by read_file.
func SetSpillReader(fn func(ref string) (string, error)) { spillReader = fn }

// ---- write_file ----

type writeFileArgs struct {
	fileArgs
	Content string `json:"content"`
}

type WriteFile struct{ confirmer Confirmer }

// setConfirmer implements confirmerHolder (pointer receiver so the
// registry mutation is visible to Call).
func (w *WriteFile) setConfirmer(c Confirmer) { w.confirmer = c }

func (*WriteFile) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name:        "write_file",
			Description: "Create or overwrite a text file. Parent directories are created automatically.",
			Parameters: object(
				`"path":{"type":"string"},"content":{"type":"string"}`,
				"path", "content"),
		},
	}
}

func (w *WriteFile) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a writeFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	if isLocksFile(a.Path) {
		if err := confirmDanger(w.confirmer, "lock-file write", a.Path); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (%d bytes)", a.Path, len(a.Content)), nil
}

// ---- edit_file ----

type editFileArgs struct {
	fileArgs
	Old    string `json:"old_string"`
	New    string `json:"new_string"`
	Expect string `json:"expect_sha256"`
}

type EditFile struct{ confirmer Confirmer }

// setConfirmer implements confirmerHolder.
func (e *EditFile) setConfirmer(c Confirmer) { e.confirmer = c }

func (*EditFile) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name: "edit_file",
			Description: "Exact string replacement in a file. old_string must match exactly once. " +
				"expect_sha256 must be the hash from your last read_file of this path; " +
				"if the file changed since, the edit is rejected and the latest fragment is returned.",
			Parameters: object(
				`"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},`+
					`"expect_sha256":{"type":"string"}`,
				"path", "old_string", "new_string", "expect_sha256"),
		},
	}
}

func (e *EditFile) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a editFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	if a.Expect == "" {
		return "", fmt.Errorf("expect_sha256 is required; read_file the path first and pass its [sha256:...] marker")
	}
	if isLocksFile(a.Path) {
		if err := confirmDanger(e.confirmer, "lock-file write", a.Path); err != nil {
			return "", err
		}
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	content := string(data)
	if cur := sha256Hex(content); cur != a.Expect {
		return "", fmt.Errorf("file changed since read (expected sha256 %s, current %s); latest fragment:\n%s",
			a.Expect, cur, fragment(content, a.Old))
	}
	count := strings.Count(content, a.Old)
	switch count {
	case 0:
		return "", fmt.Errorf("old_string not found in %s", a.Path)
	case 1:
	default:
		return "", fmt.Errorf("old_string matches %d times in %s; add more context to make it unique", count, a.Path)
	}
	out := strings.Replace(content, a.Old, a.New, 1)
	if err := os.WriteFile(a.Path, []byte(out), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s", a.Path), nil
}

// ---- list_dir ----

type ListDir struct{}

func (ListDir) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name:        "list_dir",
			Description: "List a directory's entries (name, dir/file marker, size).",
			Parameters:  object(`"path":{"type":"string"}`, "path"),
		},
	}
}

func (ListDir) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a fileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(a.Path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
		} else {
			fmt.Fprintf(&b, "%s (%d bytes)\n", e.Name(), info.Size())
		}
	}
	return clamp(b.String()), nil
}
