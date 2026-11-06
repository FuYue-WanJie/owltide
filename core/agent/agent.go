// Package agent implements the single-agent loop:
// understand -> call tools -> read results -> continue or answer.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/guard"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// Options wire the agent together.
type Options struct {
	Config    config.Config
	Client    *model.Client
	Tools     *tool.Registry
	Decisions *decision.Logger // optional
	SessionID string
	WorkDir   string
	// OnDelta streams assistant text to the UI; optional.
	OnDelta func(text string)
	// Guard enforces protected-file integrity across tool calls; nil
	// disables the check (tests).
	Guard *guard.Guard
}

// Agent runs the loop.
type Agent struct {
	opts       Options
	messages   []model.Message
	tokens     int
	turn       int
	lastCached int
	fatalGuard bool // restore failure: stop the loop
	// lastSnap is the most recent trusted protected-set snapshot.
	lastSnap   guard.Snapshot
	prefixHash string // stable request prefix (system + tools)
}

// New creates an Agent with the system prompt in place. The request
// prefix (system message + tool definitions, in registration order)
// is byte-stable for the whole session so providers can cache it.
func New(opts Options) *Agent {
	sys := opts.Config.System
	if opts.WorkDir != "" {
		sys += "\nWorking directory: " + opts.WorkDir
	}
	a := &Agent{opts: opts}
	a.messages = []model.Message{{Role: model.RoleSystem, Content: sys}}
	a.prefixHash = a.computePrefixHash()
	return a
}

// computePrefixHash hashes the deterministic request prefix:
// the system message followed by the tool schemas in registration
// order. Struct JSON marshalling is deterministic (fields in
// declaration order, no maps).
func (a *Agent) computePrefixHash() string {
	prefix := struct {
		System model.Message `json:"system"`
		Tools  []tool.Definition `json:"tools"`
	}{System: a.messages[0]}
	if defs := a.opts.Tools.Definitions(); len(defs) > 0 {
		prefix.Tools = defs
	}
	sum := sha256.Sum256(prefixJSON(&prefix))
	return hex.EncodeToString(sum[:])
}

// prefixJSON marshals with sorted keys for full determinism.
func prefixJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	_ = enc.Encode(v)
	return b.Bytes()
}

// maxTurnsFallback guards against an unset budget.
func (a *Agent) maxTurns() int {
	if a.opts.Config.Budget.MaxTurns > 0 {
		return a.opts.Config.Budget.MaxTurns
	}
	return 30
}

// maxTokensFallback guards against an unset budget.
func (a *Agent) maxTokens() int {
	if a.opts.Config.Budget.MaxTokens > 0 {
		return a.opts.Config.Budget.MaxTokens
	}
	return 400_000
}

// Run executes one user task to completion and returns the final text.
func (a *Agent) Run(ctx context.Context, userText string) (string, error) {
	start := time.Now()
	a.turn++

	a.messages = append(a.messages, model.Message{Role: model.RoleUser, Content: userText})

	var (
		final      string
		toolRecs   []decision.ToolRecord
		runErr     error
		steps      int
		budgetStop string // non-empty when a budget cap forced the wrap-up
	)
	for {
		if ctx.Err() != nil {
			runErr = ctx.Err()
			break
		}
		// Delayed-write check: tampering may land after the tool
		// returned (nohup ... &). Re-verify against the last trusted
		// snapshot before every model call.
		if a.opts.Guard != nil && a.lastSnap != nil {
			if v, f := a.opts.Guard.Verify(a.lastSnap); len(v) > 0 || len(f) > 0 {
				runErr = fmt.Errorf("protected files were modified outside tool calls; run aborted")
				break
			}
		}
		steps++
		if steps > a.maxTurns() {
			budgetStop = "turn_limit"
			final = a.wrapUp("turn limit reached")
			break
		}
		if a.tokens > a.maxTokens() {
			budgetStop = "token_budget"
			final = a.wrapUp("token budget exhausted")
			break
		}

		if a.fatalGuard {
			final = a.wrapUp("guard restore failure; run aborted for safety")
			runErr = fmt.Errorf("guard restore failed; run aborted")
			break
		}
		msg, usage, err := a.step(ctx)
		a.tokens += usage.Total()
		a.lastCached = usage.CachedTokens
		if err != nil {
			runErr = err
			break
		}
		a.messages = append(a.messages, msg)

		if len(msg.ToolCalls) == 0 {
			final = msg.Content
			break
		}

		// Execute tools in order, feed results back.
		for _, tc := range msg.ToolCalls {
			rec := a.execTool(ctx, tc)
			toolRecs = append(toolRecs, rec)
		}
	}

	a.logTurn(start, userText, toolRecs, len(toolRecs) == 0, runErr, budgetStop)
	return final, runErr
}

// step makes one model call.
func (a *Agent) step(ctx context.Context) (model.Message, model.Usage, error) {
	req := model.Request{
		Model:    a.opts.Config.Model.Name,
		Messages: a.messages,
	}
	if defs := a.opts.Tools.Definitions(); len(defs) > 0 {
		req.Tools = defsToAny(defs)
	}
	return a.opts.Client.Stream(ctx, req, func(sr model.StreamResult) {
		if a.opts.OnDelta != nil {
			a.opts.OnDelta(sr.Content)
		}
	})
}

// execTool runs one tool call and appends the tool result message.
// Protected files are hash-checked before/after: any tool (including
// run_shell) that mutates them gets its result replaced by a violation
// error and the files are restored from the config store.
func (a *Agent) execTool(ctx context.Context, tc model.ToolCall) decision.ToolRecord {
	rec := decision.ToolRecord{Name: tc.Function.Name}
	start := time.Now()

	var before guard.Snapshot
	if a.opts.Guard != nil {
		snap, err := a.opts.Guard.Snapshot()
		if err != nil {
			// Fail closed: refuse the tool instead of skipping the guard.
			rec := decision.ToolRecord{Name: tc.Function.Name, Error: "snapshot failed: " + err.Error()}
			a.messages = append(a.messages, model.Message{
				Role: model.RoleTool, Name: tc.Function.Name,
				ToolCallID: tc.ID,
				Content:    "tool refused: protected-file snapshot unavailable: " + err.Error(),
			})
			return rec
		}
		before = snap
		a.lastSnap = snap
	}

	result, err := "tool not found", fmt.Errorf("tool %q not found", tc.Function.Name)
	if t, ok := a.opts.Tools.Get(tc.Function.Name); ok {
		if a.opts.Guard != nil && before == nil {
			// Cannot take a snapshot: refuse to run the tool at all.
			err = fmt.Errorf("protected-file snapshot unavailable; tool %q refused (fail closed)", tc.Function.Name)
		} else {
			result, err = t.Call(ctx, json.RawMessage(tc.Function.Arguments))
		}
	}

	if a.opts.Guard != nil && before != nil {
		violated, failed := a.opts.Guard.Verify(before)
		if len(violated) > 0 {
			violation := fmt.Sprintf(
				"PROTECTED FILE VIOLATION: %s was modified by tool %q and restored to the trusted snapshot",
				strings.Join(violated, ", "), tc.Function.Name)
			if rec.Error == "" {
				rec.Error = violation
				rec.OK = false
			}
			result = "tool output discarded.\n" + violation
			err = fmt.Errorf("%s", violation)
		}
		if len(failed) > 0 {
			// Restore failed: the config state cannot be trusted any
			// more this turn; abort the whole run.
			rec.Error = "guard restore failed: " + strings.Join(failed, "; ")
			result = "FATAL: " + rec.Error
			err = fmt.Errorf("%s", rec.Error)
			a.fatalGuard = true
		}
	}

	rec.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Error = err.Error()
		result = result + "\nERROR: " + err.Error()
	} else {
		rec.OK = true
	}
	if strings.TrimSpace(result) == "" {
		result = "(empty output)"
	}

	a.messages = append(a.messages, model.Message{
		Role: model.RoleTool, Name: tc.Function.Name,
		ToolCallID: tc.ID, Content: result,
	})
	return rec
}

// wrapUp asks the model to finish within constraints.
func (a *Agent) wrapUp(reason string) string {
	a.messages = append(a.messages, model.Message{
		Role: model.RoleUser,
		Content: fmt.Sprintf(
			"System: %s. Stop working and give a brief final summary of what was done.", reason),
	})
	msg, usage, err := a.step(context.Background())
	a.tokens += usage.Total()
	if err != nil {
		return "(failed to produce summary: " + err.Error() + ")"
	}
	a.messages = append(a.messages, msg)
	return msg.Content
}

// logTurn writes the decision record. budgetStop names the cap that
// forced the wrap-up ("turn_limit" | "token_budget"), empty otherwise;
// it is recorded as an error-outcome so truncation is visible in the log.
func (a *Agent) logTurn(start time.Time, input string, tools []decision.ToolRecord, repliedDirectly bool, err error, budgetStop string) {
	if a.opts.Decisions == nil {
		return
	}
	outcome := decision.OutcomeSuccess
	errMsg := ""
	switch {
	case err != nil:
		outcome = decision.OutcomeError
		errMsg = err.Error()
	case budgetStop != "":
		outcome = decision.OutcomeError
		errMsg = "budget stop: " + budgetStop + " (forced wrap-up)"
	}
	rec := decision.Record{
		SessionID:       a.opts.SessionID,
		Turn:            a.turn,
		InputDigest:     digest(input),
		Model:           a.opts.Config.Model.Name,
		ToolsCalled:     tools,
		RepliedDirectly: repliedDirectly,
		DurationMS:      time.Since(start).Milliseconds(),
		Outcome:         outcome,
		Error:           errMsg,
		SchemaV2:        true,
		PrefixHash:      a.prefixHash,
		CachedTokens:    a.lastCached,
	}
	_ = a.opts.Decisions.Log(rec)
}

// digest keeps the first 200 runes of user input.
func digest(s string) string {
	r := []rune(s)
	if len(r) <= 200 {
		return s
	}
	return string(r[:200]) + "..."
}

// defsToAny widens definitions for JSON encoding.
func defsToAny(defs []tool.Definition) []any {
	out := make([]any, len(defs))
	for i, d := range defs {
		out[i] = d
	}
	return out
}
