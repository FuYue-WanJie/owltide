package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/tool"
)

func writeDef(t *testing.T, root, name, body string) {
	t.Helper()
	os.MkdirAll(Dir(root), 0o755)
	if err := os.WriteFile(filepath.Join(Dir(root), name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const mdDef = `---
name: reviewer
description: Reviews Go code
model: m
tools:
    - read_file
    - write_file
budget:
    max_turns: 5
    max_tokens: 20000
    tool_timeout_sec: 30
---

You review Go code.
`

func TestLoadMarkdownFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeDef(t, root, "reviewer.md", mdDef)
	defs, err := Load(root, map[string]bool{"read_file": true, "write_file": true})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := defs["reviewer"]
	if !ok {
		t.Fatalf("reviewer not loaded: %v", defs)
	}
	if d.Description != "Reviews Go code" || d.SystemPrompt != "You review Go code." {
		t.Fatalf("bad parse: %+v", d)
	}
	if d.Budget.MaxTurns != 5 || len(d.Tools) != 2 {
		t.Fatalf("bad fields: %+v", d)
	}
}

func TestLoadRejectsUnknownToolAndMissingDescription(t *testing.T) {
	root := t.TempDir()
	bad := strings.Replace(mdDef, "- write_file", "- nuke_everything", 1)
	writeDef(t, root, "bad1.md", bad)
	if _, err := Load(root, map[string]bool{"read_file": true}); err == nil {
		t.Fatal("unknown tool must be rejected")
	}
	root2 := t.TempDir()
	noDesc := strings.Replace(mdDef, "description: Reviews Go code", "description: \"\"", 1)
	writeDef(t, root2, "bad2.md", noDesc)
	if _, err := Load(root2, nil); err == nil {
		t.Fatal("missing description must be rejected")
	}
}

func TestSaveRenderRoundtrip(t *testing.T) {
	root := t.TempDir()
	d := Def{
		Name: "coder", Description: "writes code", Model: "m",
		SystemPrompt: "You write code.", Tools: []string{"read_file"},
		Budget: Budget{MaxTurns: 3, MaxTokens: 100, ToolTimeoutSec: 9},
	}
	if err := Save(root, d); err != nil {
		t.Fatal(err)
	}
	defs, err := Load(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := defs["coder"]
	if got.Description != d.Description || got.SystemPrompt != d.SystemPrompt ||
		got.Budget != d.Budget || len(got.Tools) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestFilterRegistryWhitelist(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(tool.ReadFile{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(tool.ListDir{}); err != nil {
		t.Fatal(err)
	}
	d := Def{Tools: []string{"read_file"}}
	filtered, err := d.FilterRegistry(reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := filtered.Get("read_file"); !ok {
		t.Fatal("whitelisted tool missing")
	}
	if _, ok := filtered.Get("list_dir"); ok {
		t.Fatal("non-whitelisted tool leaked into filtered registry")
	}
	// A whitelist naming an unregistered tool fails loudly.
	d2 := Def{Tools: []string{"nope"}}
	if _, err := d2.FilterRegistry(reg); err == nil {
		t.Fatal("unknown whitelist tool must fail")
	}
}

// TestSeedBuiltinsVersionUpgrade: a v1 (pre-versioning) untouched role
// file is upgraded in place; a user-edited file is left alone with a
// note; a current-version file is not rewritten.
func TestSeedBuiltinsVersionUpgrade(t *testing.T) {
	root := t.TempDir()

	// 1. Fresh seed: missing files are written with the current version.
	if _, err := SeedBuiltins(root); err != nil {
		t.Fatal(err)
	}
	explorePath := filepath.Join(Dir(root), "explore.md")
	data, _ := os.ReadFile(explorePath)
	if !strings.Contains(string(data), "builtin_version: 3") {
		t.Fatalf("fresh seed must stamp builtin_version: 3:\n%s", data)
	}

	// 2. Untouched v1 file (hash matches a shipped older version):
	// upgraded to the current render.
	v1Explore := Def{
		Name: "explore", Description: "Read-only scout: files and directory listings plus a parser-gated read-only shell. Parallelizable.",
		Model: "inherit", Readonly: true,
		SystemPrompt: "You are the OwlTide scout. Answer with facts you verified by reading " +
			"files and running read-only commands. Never speculate; quote what you saw. " +
			"Finish with a compact findings summary.",
		Tools:          []string{"read_file", "list_dir", "run_shell"},
		Budget:         Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
		Builtin:        true,
		BuiltinVersion: 0, // pre-versioning files carry none
	}
	if err := os.WriteFile(explorePath, []byte(v1Explore.render()), 0o600); err != nil {
		t.Fatal(err)
	}
	notes, err := SeedBuiltins(root)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(explorePath)
	if !strings.Contains(string(data), "Work inside the path the task gives") {
		t.Fatalf("v1 untouched file must be upgraded to v2 prompt:\n%s", data)
	}
	for _, n := range notes {
		if strings.Contains(n, "explore") {
			t.Fatalf("upgraded file must not produce a user-edit note: %v", notes)
		}
	}

	// 3. User-edited file (content matches no shipped version): untouched.
	edited := strings.Replace(string(data), "OwlTide scout", "OwlTide scout (customized)", 1)
	if err := os.WriteFile(explorePath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	notes, err = SeedBuiltins(root)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(explorePath)
	if !strings.Contains(string(data), "customized") {
		t.Fatalf("user-edited file must not be overwritten:\n%s", data)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "explore") {
			found = true
		}
	}
	if !found {
		t.Fatalf("edited file must produce a note: %v", notes)
	}
}

// TestWorkerBuiltinExists: router mode needs a writable role; worker is
// seeded among the builtins with run_shell and editing tools.
func TestWorkerBuiltinExists(t *testing.T) {
	var worker *Def
	for _, d := range BuiltinRoles() {
		if d.Name == "worker" {
			worker = &d
		}
	}
	if worker == nil {
		t.Fatal("worker role missing from builtins")
	}
	if worker.Readonly {
		t.Fatal("worker must be writable")
	}
	has := map[string]bool{}
	for _, tl := range worker.Tools {
		has[tl] = true
	}
	for _, need := range []string{"run_shell", "edit_file", "write_file"} {
		if !has[need] {
			t.Fatalf("worker missing %q", need)
		}
	}
}
