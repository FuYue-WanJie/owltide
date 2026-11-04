package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
			Name:        "read_file",
			Description: "Read a text file. Returns the full content (truncated if huge).",
			Parameters:  object(`"path":{"type":"string"}`, "path"),
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
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	return clamp(string(data)), nil
}

// ---- write_file ----

type writeFileArgs struct {
	fileArgs
	Content string `json:"content"`
}

type WriteFile struct{}

func (WriteFile) Schema() Definition {
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

func (WriteFile) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a writeFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
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
	Old string `json:"old_string"`
	New string `json:"new_string"`
}

type EditFile struct{}

func (EditFile) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name:        "edit_file",
			Description: "Exact string replacement in a file. old_string must match exactly once.",
			Parameters: object(
				`"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}`,
				"path", "old_string", "new_string"),
		},
	}
}

func (EditFile) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a editFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	content := string(data)
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
