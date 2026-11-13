// Package tool defines the tool interface and the built-in tools.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Definition is the OpenAI-style schema sent to the model.
type Definition struct {
	Type     string         `json:"type"`
	Function FunctionSchema `json:"function"`
}

// FunctionSchema describes one function tool.
type FunctionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Tool is a callable capability. Arguments arrive as raw JSON from
// the model; implementations parse what they need.
type Tool interface {
	Schema() Definition
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Capability declares what a tool can do. Any tool whose Schemaer
// does not implement Capability is treated as WRITING (fail closed):
// read-only roles only receive explicitly read-only tools.
type Capability interface {
	Caps() Caps
}

// Caps is a bitmask of side-effect classes.
type Caps uint8

const (
	CapReads   Caps = 1 << iota // pure reads
	CapWrites                   // mutates files/config state
	CapExec                     // runs subprocesses / shell
	CapNetwork                  // talks to the network
)

// String renders the set for logs.
func (c Caps) String() string {
	var b []string
	if c&CapReads != 0 {
		b = append(b, "reads")
	}
	if c&CapWrites != 0 {
		b = append(b, "writes")
	}
	if c&CapExec != 0 {
		b = append(b, "exec")
	}
	if c&CapNetwork != 0 {
		b = append(b, "network")
	}
	if len(b) == 0 {
		return "none"
	}
	return strings.Join(b, ",")
}

// CloneConfirmer is implemented by tools that can produce a copy with
// their own confirmer. Registries use it to give each sub-agent
// session its own confirmation stream (labeled, serialized) while the
// main agent's tools keep theirs.
type CloneConfirmer interface {
	CloneWithConfirmer(c Confirmer) Tool
}
type Registry struct {
	tools     map[string]Tool
	order     []string
	confirmer Confirmer
	caps      map[string]Caps
}

// capsOf resolves a tool's declared capabilities (none => writes).
func capsOf(t Tool) Caps {
	if c, ok := t.(Capability); ok {
		return c.Caps()
	}
	return CapWrites
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, caps: map[string]Caps{}}
}

// Register adds a tool; duplicate names fail loudly.
func (r *Registry) Register(t Tool) error {
	name := t.Schema().Function.Name
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool %q already registered", name)
	}
	r.tools[name] = Tool(t)
	r.order = append(r.order, name)
	r.caps[name] = capsOf(t)
	if h, ok := t.(confirmerHolder); ok && r.confirmer != nil {
		h.setConfirmer(r.confirmer)
	}
	return nil
}

// Caps returns the declared capabilities of a registered tool.
func (r *Registry) Caps(name string) (Caps, bool) {
	c, ok := r.caps[name]
	return c, ok
}

// ReadOnlyView returns a registry containing only tools declared as
// pure reads. Undeclared tools are treated as writes and excluded,
// except keep — tools whose writes are confined to the session's own
// workspace (e.g. todo.md) and are therefore safe for read-only roles.
func (r *Registry) ReadOnlyView(keep ...string) *Registry {
	keepSet := map[string]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}
	out := NewRegistry()
	for _, name := range r.order {
		c := r.caps[name]
		if c == CapReads || keepSet[name] {
			_ = out.Register(r.tools[name])
		}
	}
	return out
}

// SetConfirmer wires the user-confirmation callback into every tool
// that needs it (dangerous commands, lock-file writes).
func (r *Registry) SetConfirmer(c Confirmer) {
	r.confirmer = c
	for _, t := range r.tools {
		if h, ok := t.(confirmerHolder); ok {
			h.setConfirmer(c)
		}
	}
}

// SetDetailConfirmer wires the structured (kind/detail) confirmation
// callback; tools that support it prefer it over the plain Confirmer.
func (r *Registry) SetDetailConfirmer(c DetailConfirmer) {
	for _, t := range r.tools {
		if h, ok := t.(detailConfirmerHolder); ok {
			h.setDetailConfirmer(c)
		}
	}
}

// CloneWithConfirmer returns a copy of the registry in which every
// tool that can carry a confirmer gets its own clone wired to c.
// Shared tool instances in the source registry are untouched, so the
// main agent's confirmation stream stays independent.
func (r *Registry) CloneWithConfirmer(c Confirmer) *Registry {
	out := NewRegistry()
	for _, name := range r.order {
		t := r.tools[name]
		if cl, ok := t.(CloneConfirmer); ok {
			t = cl.CloneWithConfirmer(c)
		}
		_ = out.Register(t)
	}
	return out
}

// Get returns the tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Definitions returns schemas in registration order (stable for tests).
func (r *Registry) Definitions() []Definition {
	out := make([]Definition, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n].Schema())
	}
	return out
}

// object builds a JSON schema snippet for object parameters.
func object(props string, required ...string) json.RawMessage {
	req := ""
	for i, s := range required {
		if i > 0 {
			req += ","
		}
		req += `"` + s + `"`
	}
	return json.RawMessage(fmt.Sprintf(
		`{"type":"object","properties":{%s},"required":[%s]}`, props, req))
}
