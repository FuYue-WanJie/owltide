package evolve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// HistoryRollback reverts the working tree to an earlier commit.
// Locked items are protected: the revert is applied, then the current
// (newer) contents of locked paths are restored on top before commit.
type HistoryRollback struct{ Env *Env }

// Schema implements tool.Tool.
func (HistoryRollback) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name:        "history_rollback",
		Description: "Roll the configuration back to a previous commit by its id (short ids ok). Changes to locked items are preserved.",
		Parameters: json.RawMessage(`{"type":"object",
 "properties":{"commit":{"type":"string"},"steps":{"type":"integer"}},
 "required":[]}`),
	}}
}

// Call implements tool.Tool.
func (t HistoryRollback) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Commit string `json:"commit"`
		Steps  int    `json:"steps"`
	}
	_ = json.Unmarshal(args, &a)

	log, err := t.Env.St.Log()
	if err != nil || len(log) == 0 {
		return reject("no history to roll back")
	}
	target := a.Commit
	if target == "" {
		if a.Steps <= 0 {
			a.Steps = 1
		}
		if a.Steps >= len(log) {
			return reject("cannot roll back past the first commit")
		}
		target = log[a.Steps].ID
	}
	full, err := t.Env.St.ResolveCommit(target)
	if err != nil {
		return reject("commit %q not found", target)
	}
	// Refuse to touch locked paths: stash + journal them in a
	// crash-safe commit before mutating the working tree.
	protected, err := t.Env.stashLocked()
	if err != nil {
		return reject("%v", err)
	}
	if err := t.Env.St.JournalLockedStash(protected, t.Env.SessionID); err != nil {
		return reject("stash journal failed: %v", err)
	}
	meta := store.CommitMeta{Who: "agent", Tool: "history.rollback",
		Game: t.Env.SessionID, Reason: "rollback to " + short(full)}
	prevHead, _ := t.Env.St.Head()
	if err := t.Env.St.Checkout(full); err != nil {
		return reject("checkout failed: %v", err)
	}
	t.Env.restoreLocked(protected)
	// Rollback finished: drop the crash-safety journal.
	t.Env.St.ClearLockedStash()
	commitID, changed, err := t.Env.St.AutoCommit(meta)
	if err != nil {
		t.Env.tryRevert(prevHead)
		return reject("rollback failed: %v", err)
	}
	if !changed {
		// Working tree already matches the target (e.g. only locked
		// paths differed and were preserved) — the goal state holds.
		return "already at target state", nil
	}
	if t.Env.Load != nil {
		if err := t.Env.Load(t.Env.Root); err != nil {
			t.Env.tryRevert(prevHead)
			return reject("reload failed, rolled back: %v", err)
		}
	}
	if t.Env.Health != nil {
		if err := t.Env.Health(t.Env.Root); err != nil {
			t.Env.tryRevert(prevHead)
			return reject("health check failed, rolled back: %v", err)
		}
	}
	return fmt.Sprintf("rolled back to %s (%s)", short(commitID), short(full)), nil
}

// stashLocked reads every locked path's current bytes (top level files
// and agents/*.yaml of locked agents).
func (e *Env) stashLocked() (map[string][]byte, error) {
	out := map[string][]byte{}
	locks, err := e.St.LoadLocks()
	if err != nil {
		return nil, err
	}
	for _, id := range append(locks.Items, store.Permanent()...) {
		for _, p := range pathsForLockID(e.Root, id) {
			if data, err := readFileIfExists(p); err == nil {
				rel, _ := filepathRel(e.Root, p)
				out[rel] = data
			}
		}
	}
	return out, nil
}

func (e *Env) restoreLocked(protected map[string][]byte) {
	for rel, data := range protected {
		p := joinPath(e.Root, rel)
		mkdirParent(p)
		_ = osWriteFile(p, data)
	}
}

// pathsForLockID maps a lock id to concrete files.
func pathsForLockID(root, id string) []string {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	switch parts[0] {
	case "config":
		if parts[1] == "budget" || parts[1] == "locks" {
			return []string{joinPath(root, "config.yaml")}
		}
		return nil
	case "locks":
		return []string{joinPath(root, "locks.yaml")}
	case "agent":
		return []string{joinPath(root, "agents", parts[1]+".md")}
	case "skill":
		return []string{joinPath(root, "skills", parts[1]+".md")}
	case "memory":
		return []string{joinPath(root, "memory", parts[1]+".md")}
	}
	return nil
}

// ChangesReport renders the change journal since a duration.
func ChangesReport(st *store.Store, since time.Duration) (string, error) {
	log, err := st.Log()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	cutoff := time.Now().Add(-since)
	n := 0
	for _, c := range log {
		if n >= 200 {
			break
		}
		when := parseWhen(c.When)
		if !when.IsZero() && when.Before(cutoff) {
			break
		}
		reason, who, toolName, session := store.ParseMessage(c.Msg)
		fmt.Fprintf(&b, "%s  %s  who=%s tool=%s session=%s\n", c.When[:16], short(c.ID), who, toolName, session)
		fmt.Fprintf(&b, "    %s\n", reason)
		if len(c.Changed) > 0 {
			fmt.Fprintf(&b, "    files: %s\n", strings.Join(c.Changed, ", "))
		}
		n++
	}
	if n == 0 {
		return "no changes in the period", nil
	}
	return b.String(), nil
}
