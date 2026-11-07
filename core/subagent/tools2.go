package subagent

// agent_status / agent_send / agent_stop / agent_result.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/FuYue-WanJie/owltide/core/tool"
)

// ---- agent_status ----

type agentStatus struct{ m *Manager }

func (agentStatus) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name:        "agent_status",
		Description: "Report the state of a sub-agent (queued|running|done|stopped|failed).",
		Parameters:  objSchema(`"id":{"type":"string"}`, "id"),
	}}
}

func (t agentStatus) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ ID string }
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	t.m.mu.Lock()
	s, ok := t.m.sessions[a.ID]
	t.m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q", a.ID)
	}
	b, _ := json.Marshal(map[string]string{
		"id": s.ID, "role": s.Role, "state": s.State,
		"started_at": s.Started.UTC().Format("2006-01-02T15:04:05Z"),
	})
	return string(b), nil
}

// ---- agent_send ----

type agentSend struct{ m *Manager }

func (agentSend) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name:        "agent_send",
		Description: "Send a follow-up message to a sub-agent. For a finished/stopped sub-agent this resumes it from its own JSONL log with fresh budget.",
		Parameters:  objSchema(`"id":{"type":"string"},"message":{"type":"string"}`, "id", "message"),
	}}
}

func (t agentSend) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID      string
		Message string
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	t.m.mu.Lock()
	s, ok := t.m.sessions[a.ID]
	t.m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q", a.ID)
	}
	if s.State == StateRunning || s.State == StateQueued {
		return "", fmt.Errorf("sub-agent %s is still %s; use agent_result to collect", s.ID, s.State)
	}
	// Resume: rebuild a fresh agent from the same role/task and the
	// previous summary as context. Budget is re-deducted by caller.
	s.State = StateQueued
	if !s.Def.Readonly {
		t.m.transfer(s.ID)
	}
	go func() {
		sum, err := t.m.runSession(ctx, s)
		t.m.deliver(s)
		t.m.event("finished", map[string]any{"id": s.ID, "role": s.Role, "state": s.State, "resumed": true})
		_ = sum
		_ = err
	}()
	b, _ := json.Marshal(map[string]any{"id": s.ID, "accepted": true, "resumed": true})
	return string(b), nil
}

// ---- agent_stop ----

type agentStop struct{ m *Manager }

func (agentStop) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name:        "agent_stop",
		Description: "Stop a sub-agent (and, transitively, anything it started). Returns the ids stopped.",
		Parameters:  objSchema(`"id":{"type":"string"}`, "id"),
	}}
}

func (t agentStop) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ ID string }
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	t.m.mu.Lock()
	s, ok := t.m.sessions[a.ID]
	t.m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q", a.ID)
	}
	var stopped []string
	if s.Cancel != nil {
		s.Cancel()
	}
	if s.State == StateRunning || s.State == StateQueued {
		s.State = StateStopped
		stopped = append(stopped, s.ID)
		t.m.transfer("main") // token returns on stop
		t.m.event("stopped", map[string]any{"id": s.ID, "by": "agent_stop"})
	}
	b, _ := json.Marshal(map[string]any{"id": s.ID, "stopped": stopped})
	return string(b), nil
}

// ---- agent_result ----

type agentResult struct{ m *Manager }

func (agentResult) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name:        "agent_result",
		Description: "Collect a sub-agent's summary. Results are delivered exactly once (auto after sync start, or via the main log for background ones); later calls return only a reference.",
		Parameters:  objSchema(`"id":{"type":"string"}`, "id"),
	}}
}

func (t agentResult) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ ID string }
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	t.m.mu.Lock()
	s, ok := t.m.sessions[a.ID]
	t.m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q", a.ID)
	}
	if s.State == StateRunning || s.State == StateQueued {
		b, _ := json.Marshal(map[string]string{"id": s.ID, "status": s.State})
		return string(b), nil
	}
	// Delivered-once: after the first delivery the full summary lives
	// in the main log; repeat calls get the reference only.
	if s.Delivered {
		b, _ := json.Marshal(map[string]string{
			"id": s.ID, "status": s.State,
			"note": "result already delivered to the main log; see the [subagent " + s.ID + " result] event there",
		})
		return string(b), nil
	}
	// Not yet delivered (finished in the background): deliver now —
	// the summary enters the main log once, and this call returns the
	// summary inline as well.
	t.m.deliver(s)
	sum := t.m.foldLarge(s.Summary)
	b, _ := json.Marshal(map[string]any{
		"id": s.ID, "status": s.State, "summary": sum,
	})
	return string(b), nil
}
