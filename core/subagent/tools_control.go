package subagent

// agent_status / agent_send (resume) / agent_stop / agent_result:
// the control tools for already-started sub-agent sessions.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/agent"
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
	state := ""
	if ok {
		state = s.State
	}
	t.m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q", a.ID)
	}
	fields := map[string]string{
		"id": s.ID, "role": s.Role, "state": s.State,
		"started_at": s.Started.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if state == StateRunning || state == StateQueued {
		// Same-turn polling is the loop-detector trap: tell the model
		// to end its turn instead of retrying.
		fields["note"] = "仍在运行，结束本轮即可，结果会自动送达"
	}
	b, _ := json.Marshal(fields)
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
	// A finished sub-agent whose result was already delivered has no
	// open thread to continue: refuse instead of silently re-running
	// with the output going nowhere (and no budget reserved). A fresh
	// agent_start is the correct follow-up.
	if s.Delivered || t.m.deliveredSeen[s.ID] {
		return "", fmt.Errorf("该子代理已结束，请用 agent_start 新开（%s 已交付结果，不重新运行）", s.ID)
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
	// Same-turn inline delivery: this call becomes THE one delivery.
	// The pair-based path (deliver) is skipped so the next turn does
	// not inject a duplicate; the summary enters the main log as this
	// call's tool result, which rebuilds correctly.
	inline := false
	if ok && s.State != StateRunning && s.State != StateQueued &&
		!s.Delivered && !t.m.deliveredSeen[s.ID] {
		if strings.TrimSpace(s.Summary) == "" && s.State != StateInterrupted {
			s.Summary = agent.BudgetExhaustedSummary
			if s.State == StateDone {
				s.State = StateBudgetExhausted
			}
		}
		s.Delivered = true
		inline = true
	}
	t.m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown sub-agent %q", a.ID)
	}
	if s.State == StateRunning || s.State == StateQueued {
		b, _ := json.Marshal(map[string]string{
			"id": s.ID, "status": s.State,
			"note": "仍在运行，结束本轮即可，结果会自动送达",
		})
		return string(b), nil
	}
	if inline {
		t.m.event("delivered", map[string]any{
			"id": s.ID, "role": s.Role, "summary_digest": digest(s.Summary),
			"via": "agent_result",
		})
		sum := t.m.foldLarge(s.Summary)
		b, _ := json.Marshal(map[string]any{
			"id": s.ID, "status": s.State, "summary": sum,
		})
		return string(b), nil
	}
	// Delivered-once: after the first delivery the full summary lives
	// in the main log; repeat calls get the reference only, and must
	// stop polling.
	b, _ := json.Marshal(map[string]string{
		"id": s.ID, "status": s.State,
		"note": "结果已在本会话前文的 agent_result 中，不要再查询",
	})
	return string(b), nil
}
