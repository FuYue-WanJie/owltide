package agents

// The four built-in roles, seeded into ~/.owltide/agents on init and
// on first load when missing. Users may edit them but never delete
// them; self-evolution may never widen their tool whitelist or drop
// their readonly flag.

import (
	"fmt"
	"os"
	"path/filepath"
)

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
// Existing user edits are never overwritten.
func SeedBuiltins(root string) error {
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, d := range BuiltinRoles() {
		d.Builtin = true
		path := filepath.Join(dir, d.Name+".md")
		if _, err := os.Stat(path); err == nil {
			continue // user may have edited it; keep
		}
		if err := os.WriteFile(path, []byte(d.render()), 0o600); err != nil {
			return fmt.Errorf("seed %s: %w", d.Name, err)
		}
	}
	return nil
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
