// Package tool defines the tool interface and the built-in tools.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
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

// Registry holds tools by name.
type Registry struct {
	tools     map[string]Tool
	order     []string
	confirmer Confirmer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool; duplicate names fail loudly.
func (r *Registry) Register(t Tool) error {
	name := t.Schema().Function.Name
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool %q already registered", name)
	}
	r.tools[name] = Tool(t)
	r.order = append(r.order, name)
	if h, ok := t.(confirmerHolder); ok && r.confirmer != nil {
		h.setConfirmer(r.confirmer)
	}
	return nil
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
