// Package evolve implements the self-evolution tools with a uniform
// transaction: validate -> lock check -> write -> git commit ->
// hot reload -> health check; any failure reverts to the previous
// commit and returns the reason as the tool result.
package evolve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/agents"
	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// Loader re-reads configuration after a successful commit.
type Loader func(root string) error

// Health checks that the new state is usable; return an error to roll back.
type Health func(root string) error

// Env wires the evolution tools together.
type Env struct {
	Root      string // ~/.owltide
	St        *store.Store
	Load      Loader
	Health    Health
	SessionID string
	Who       string // always "agent" for these tools

	// KnownToolsSet is the registered tool-name set used to validate
	// agent.upsert tool lists.
	KnownToolsSet map[string]bool
}

// RejectErr marks lock/validation rejections with a stable prefix.
const RejectErr = "REJECTED: "

func reject(format string, a ...any) (string, error) {
	return "", fmt.Errorf(RejectErr+format, a...)
}

// commitWithRollback writes fn, commits, reloads, health-checks;
// reverts on failure after the write.
func (e *Env) commitWithRollback(ctx context.Context, meta store.CommitMeta, fn func() error) (string, error) {
	// Lock checks happen inside fn before touching files.
	prevHead, _ := e.St.Head()
	if err := fn(); err != nil {
		return reject("%v", err)
	}
	commitID, changed, err := e.St.AutoCommit(meta)
	if err != nil {
		e.tryRevert(prevHead)
		return "", fmt.Errorf("commit failed: %w", err)
	}
	if !changed {
		return "no changes", nil
	}
	if e.Load != nil {
		if err := e.Load(e.Root); err != nil {
			e.tryRevert(prevHead)
			return reject("reload failed, rolled back: %v", err)
		}
	}
	if e.Health != nil {
		if err := e.Health(e.Root); err != nil {
			e.tryRevert(prevHead)
			return reject("health check failed, rolled back: %v", err)
		}
	}
	_ = ctx
	return fmt.Sprintf("ok (%s)", short(commitID)), nil
}

func (e *Env) tryRevert(prevHead string) {
	if prevHead == "" {
		return
	}
	if c, err := e.St.ExportLoadCommit(prevHead); err == nil {
		_ = e.St.Checkout(c.ID)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// guardLock rejects when id is locked.
func (e *Env) guardLock(id string) error {
	if e.St.IsLocked(id) {
		return fmt.Errorf("%q is locked; only the user can change it (owltide unlock)", id)
	}
	return nil
}

// ---- agent.upsert / agent.remove ----

type upsertArgs struct {
	agents.Def `yaml:",inline"`
}

// AgentUpsert creates or updates a sub-agent definition.
type AgentUpsert struct{ Env *Env }

// Schema implements tool.Tool.
func (AgentUpsert) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "agent.upsert",
		Description: "Create or update a sub-agent definition in ~/.owltide/agents. " +
			"Validated against the schema; locked agents are rejected. Fields: name, model, system_prompt, tools, budget{max_turns,max_tokens,tool_timeout_sec}, base_url(optional).",
		Parameters: json.RawMessage(`{"type":"object",
 "properties":{
   "name":{"type":"string"},
   "model":{"type":"string"},
   "base_url":{"type":"string"},
   "system_prompt":{"type":"string"},
   "tools":{"type":"array","items":{"type":"string"}},
   "budget":{"type":"object","properties":{
     "max_turns":{"type":"integer"},"max_tokens":{"type":"integer"},"tool_timeout_sec":{"type":"integer"}}}
 },
 "required":["name","model","system_prompt","tools","budget"]}`),
	}}
}

// Call implements tool.Tool.
func (t AgentUpsert) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a upsertArgs
	if err := yamlUnmarshal(args, &a); err != nil {
		return reject("bad arguments: %v", err)
	}
	if err := a.Validate(t.Env.knownTools()); err != nil {
		return reject("invalid definition: %v", err)
	}
	if err := t.Env.guardLock(a.LockID()); err != nil {
		return reject("%v", err)
	}
	meta := store.CommitMeta{Who: "agent", Tool: "agent.upsert",
		Game: t.Env.SessionID, Reason: "upsert agent " + a.Name}
	return t.Env.commitWithRollback(ctx, meta, func() error {
		return agents.Save(t.Env.Root, a.Def)
	})
}

// AgentRemove deletes a sub-agent definition.
type AgentRemove struct{ Env *Env }

// Schema implements tool.Tool.
func (AgentRemove) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name:        "agent.remove",
		Description: "Remove a sub-agent definition by name. Locked agents are rejected.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	}}
}

// Call implements tool.Tool.
func (t AgentRemove) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct{ Name string }
	if err := json.Unmarshal(args, &a); err != nil {
		return reject("bad arguments: %v", err)
	}
	if err := t.Env.guardLock("agent:" + a.Name); err != nil {
		return reject("%v", err)
	}
	meta := store.CommitMeta{Who: "agent", Tool: "agent.remove",
		Game: t.Env.SessionID, Reason: "remove agent " + a.Name}
	return t.Env.commitWithRollback(ctx, meta, func() error {
		return agents.Remove(t.Env.Root, a.Name)
	})
}

// ---- skill.write / memory.write ----

type writeArgs struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

func (e *Env) writer(toolName, dir, what string) tool.Tool {
	return writerTool{e: e, toolName: toolName, dir: dir, what: what}
}

type writerTool struct {
	e        *Env
	toolName string
	dir      string
	what     string
}

func (w writerTool) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: w.toolName,
		Description: "Write a " + w.what + " markdown file to ~/.owltide/" + w.dir +
			"/<name>.md. Locked entries are rejected.",
		Parameters: json.RawMessage(`{"type":"object",
 "properties":{"name":{"type":"string"},"body":{"type":"string"}},
 "required":["name","body"]}`),
	}}
}

func (writerTool) nameOf(a writeArgs) string { return "" }

func (w writerTool) pathOf(a writeArgs) (string, error) {
	if !validName(a.Name) {
		return "", fmt.Errorf("name %q must be a simple file stem (letters, digits, dash, dot)", a.Name)
	}
	return filepath.Join(w.e.Root, w.dir, a.Name+".md"), nil
}

func (w writerTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a writeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return reject("bad arguments: %v", err)
	}
	if strings.TrimSpace(a.Body) == "" {
		return reject("body must not be empty")
	}
	if err := w.e.guardLock(w.nameOf(a)); err != nil {
		return reject("%v", err)
	}
	p, err := w.pathOf(a)
	if err != nil {
		return reject("%v", err)
	}
	meta := store.CommitMeta{Who: "agent", Tool: w.toolName,
		Game: w.e.SessionID, Reason: "write " + w.dir + "/" + a.Name + ".md"}
	return w.e.commitWithRollback(ctx, meta, func() error {
		os.MkdirAll(filepath.Dir(p), 0o755)
		return os.WriteFile(p, []byte(a.Body), 0o600)
	})
}

// SkillWrite writes a skill markdown file.
type SkillWrite struct{ Env *Env }

// Schema implements tool.Tool.
func (SkillWrite) Schema() tool.Definition {
	return writerTool{dir: "skills", what: "skill"}.Schema()
}

// Call implements tool.Tool.
func (t SkillWrite) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return writerTool{e: t.Env, dir: "skills", what: "skill"}.Call(ctx, args)
}

// MemoryWrite writes a memory markdown file.
type MemoryWrite struct{ Env *Env }

// Schema implements tool.Tool.
func (MemoryWrite) Schema() tool.Definition {
	return writerTool{dir: "memory", what: "memory"}.Schema()
}

// Call implements tool.Tool.
func (t MemoryWrite) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return writerTool{e: t.Env, dir: "memory", what: "memory"}.Call(ctx, args)
}

// RegisterAll registers the full self-evolution toolset on reg.
func RegisterAll(reg *tool.Registry, env *Env) error {
	for _, t := range []tool.Tool{
		AgentUpsert{env}, AgentRemove{env}, SkillWrite{env},
		MemoryWrite{env}, ConfigPatch{env},
	} {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// ---- helpers ----

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return s != "." && s != ".." && !strings.HasPrefix(s, ".")
}
