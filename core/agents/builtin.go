package agents

// The four built-in roles, seeded into ~/.owltide/agents on init and
// on first load when missing. Users may edit them but never delete
// them; self-evolution may never widen their tool whitelist or drop
// their readonly flag.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// builtinVersion is stamped into every seeded role file's frontmatter
// (builtin_version). When the shipped prompts/tools change, bump it:
// existing files whose hash still matches ANY earlier shipped version
// are upgraded in place; files the user has edited are left alone.
const builtinVersion = 3

// shippedHashes maps role name -> set of sha256 hex digests of every
// builtin-rendered version of that role file (current and past).
// Versions are hashed via renderVersion, so an untouched file upgrades
// silently while an edited one never matches.
var shippedHashes = map[string]map[string]bool{}

func registerShipped(def Def) {
	d := def
	d.Builtin = true
	sum := sha256.Sum256([]byte(d.render()))
	if shippedHashes[d.Name] == nil {
		shippedHashes[d.Name] = map[string]bool{}
	}
	shippedHashes[d.Name][hex.EncodeToString(sum[:])] = true
}

func init() {
	// v1: prompts before the M2.1 focus guidance.
	registerShipped(Def{
		Name:        "general",
		Description: "Read-write worker with every tool; the only role allowed to write and the only one the write token transfers to. Not parallelizable.",
		Model:       "inherit",
		SystemPrompt: "You are the OwlTide general worker. Complete the task end to end " +
			"using the tools provided. You may create and edit files. " +
			"Finish with a short factual summary of what you did.",
		Tools:  []string{"read_file", "write_file", "edit_file", "list_dir", "run_shell", "todo"},
		Budget: Budget{MaxTurns: 15, MaxTokens: 150000, ToolTimeoutSec: 60},
	})
	registerShipped(Def{
		Name:        "explore",
		Description: "Read-only scout: files and directory listings plus a parser-gated read-only shell. Parallelizable.",
		Model:       "inherit",
		Readonly:    true,
		SystemPrompt: "You are the OwlTide scout. Answer with facts you verified by reading " +
			"files and running read-only commands. Never speculate; quote what you saw. " +
			"Finish with a compact findings summary.",
		Tools:  []string{"read_file", "list_dir", "run_shell"},
		Budget: Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
	})
	registerShipped(Def{
		Name:        "review",
		Description: "Read-only reviewer producing a structured verdict. Parallelizable.",
		Model:       "inherit",
		Readonly:    true,
		SystemPrompt: "You are the OwlTide reviewer. Read the referenced material and answer " +
			"with a structured verdict:\n" +
			"verdict: <ok|concerns|blocked>\nfindings:\n- <each with file:line and severity>\n" +
			"questions:\n- <what you could not verify>",
		Tools:  []string{"read_file", "list_dir", "run_shell"},
		Budget: Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
	})
	registerShipped(Def{
		Name:        "planner",
		Description: "Read-only planner: produces a step plan into todo.md, never executes.",
		Model:       "inherit",
		Readonly:    true,
		SystemPrompt: "You are the OwlTide planner. Produce an ordered, verifiable step plan " +
			"and write it with the todo tool. Do not execute anything; do not create or " +
			"edit project files. The final message is the plan itself.",
		Tools:  []string{"read_file", "list_dir", "todo"},
		Budget: Budget{MaxTurns: 8, MaxTokens: 60000, ToolTimeoutSec: 30},
	})
	// v2: explore focus guidance (the four original roles, current
	// content).
	// v3: adds the worker role; the four v2 roles are unchanged in
	// content, only the version header moves.
	for _, d := range BuiltinRoles() {
		registerShipped(d)
	}
}

// BuiltinRoles returns the seeded definitions. Model and budget are
// filled from config at spawn time; here they carry schema-valid
// placeholders that Load re-validates against the user's edits.
func BuiltinRoles() []Def {
	return []Def{
		{
			Name:        "general",
			Description: "Read-write worker with every tool; the only role allowed to write and the only one the write token transfers to. Not parallelizable.",
			Model:       "inherit",
			SystemPrompt: "You are the OwlTide general worker. Complete the task end to end " +
				"using the tools provided. You may create and edit files. " +
				"Finish with a short factual summary of what you did.",
			Tools:  []string{"read_file", "write_file", "edit_file", "list_dir", "run_shell", "todo"},
			Budget: Budget{MaxTurns: 15, MaxTokens: 150000, ToolTimeoutSec: 60},
		},
		{
			Name:        "explore",
			Description: "Read-only scout: files and directory listings plus a parser-gated read-only shell. Parallelizable.",
			Model:       "inherit",
			Readonly:    true,
			SystemPrompt: "You are the OwlTide scout. Work inside the path the task gives " +
				"you; if a read-only command is refused twice, stop probing other " +
				"directories and finish with what you already saw. Answer with facts " +
				"you verified by reading files and running read-only commands. Never " +
				"speculate; quote what you saw. Finish with a compact findings summary.",
			Tools:  []string{"read_file", "list_dir", "run_shell"},
			Budget: Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
		},
		{
			Name:        "review",
			Description: "Read-only reviewer producing a structured verdict. Parallelizable.",
			Model:       "inherit",
			Readonly:    true,
			SystemPrompt: "You are the OwlTide reviewer. Read the referenced material and answer " +
				"with a structured verdict:\n" +
				"verdict: <ok|concerns|blocked>\nfindings:\n- <each with file:line and severity>\n" +
				"questions:\n- <what you could not verify>",
			Tools:  []string{"read_file", "list_dir", "run_shell"},
			Budget: Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
		},
		{
			Name:        "worker",
			Description: "Read-write executor for router mode: runs shell commands and edits files under the same guard as the main agent. Not parallelizable (single writer).",
			Model:       "inherit",
			SystemPrompt: "You are the OwlTide worker. Execute the task end to end with the " +
				"tools provided; run shell commands and edit files only as the task requires. " +
				"Work inside the path the task gives you. Finish with a short factual " +
				"summary of what you changed and how you verified it.",
			Tools:  []string{"read_file", "write_file", "edit_file", "list_dir", "run_shell", "todo"},
			Budget: Budget{MaxTurns: 15, MaxTokens: 150000, ToolTimeoutSec: 60},
		},
		{
			Name:        "planner",
			Description: "Read-only planner: produces a step plan into todo.md, never executes.",
			Model:       "inherit",
			Readonly:    true,
			SystemPrompt: "You are the OwlTide planner. Produce an ordered, verifiable step plan " +
				"and write it with the todo tool. Do not execute anything; do not create or " +
				"edit project files. The final message is the plan itself.",
			Tools:  []string{"read_file", "list_dir", "todo"},
			Budget: Budget{MaxTurns: 8, MaxTokens: 60000, ToolTimeoutSec: 30},
		},
	}
}

// SeedBuiltins writes any missing built-in role file into
// ~/.owltide/agents and marks existing ones as builtin in memory.
// Existing files are upgraded only when their content is byte-identical
// to a previously shipped builtin version (builtin_version header in
// the frontmatter is informational; the hash decides); files the user
// has edited are never overwritten — the returned notes list them.
func SeedBuiltins(root string) ([]string, error) {
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var notes []string
	for _, d := range BuiltinRoles() {
		d.Builtin = true
		d.BuiltinVersion = builtinVersion
		path := filepath.Join(dir, d.Name+".md")
		existing, err := os.ReadFile(path)
		if err != nil {
			if err := os.WriteFile(path, []byte(d.render()), 0o600); err != nil {
				return notes, fmt.Errorf("seed %s: %w", d.Name, err)
			}
			continue
		}
		sum := sha256.Sum256(existing)
		hash := hex.EncodeToString(sum[:])
		if hash == "" || shippedHashes[d.Name][hash] {
			// Untouched builtin at an older version: upgrade in place.
			if !strings.Contains(string(existing), "builtin_version") ||
				!strings.Contains(string(existing), fmt.Sprintf("builtin_version: %d", builtinVersion)) {
				if err := os.WriteFile(path, []byte(d.render()), 0o600); err != nil {
					return notes, fmt.Errorf("upgrade %s: %w", d.Name, err)
				}
			}
			continue
		}
		notes = append(notes,
			d.Name+"（内容与任何内置版本都不同，判定为用户已修改，未升级）")
	}
	return notes, nil
}

// ProtectBuiltin rejects self-evolution changes that would delete a
// builtin role or widen/drop the constraints of a readonly builtin.
func ProtectBuiltin(existing, next Def) error {
	if !existing.Builtin {
		return nil
	}
	if next.Readonly != existing.Readonly {
		return fmt.Errorf("builtin role %q cannot change its readonly flag", existing.Name)
	}
	if len(next.Tools) > len(existing.Tools) {
		keep := map[string]bool{}
		for _, t := range existing.Tools {
			keep[t] = true
		}
		for _, t := range next.Tools {
			if !keep[t] {
				return fmt.Errorf("builtin role %q cannot gain tool %q", existing.Name, t)
			}
		}
	}
	return nil
}
