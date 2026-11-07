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
	"sync"
	"time"

	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/guard"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/sessionlog"
	"github.com/FuYue-WanJie/owltide/core/spill"
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
	// Spill moves oversized tool results out of the context; nil keeps
	// everything inline.
	Spill *spill.Store
	// Loop configures goal restatement and loop detection; zero uses
	// defaults. TodoDir empty disables the todo tool.
	Loop    LoopConfig
	TodoDir string
	// SessLog is the append-only main-conversation message log
	// (core/sessionlog); nil disables persistence (tests). Every
	// message appended to the in-memory history goes through logMsg,
	// the single write entry point.
	SessLog *sessionlog.Log
}

// logMsg appends one message to the in-memory history and, when
// wired, the persistent session log. This is the ONLY entry point for
// extending the conversation.
func (a *Agent) logMsg(m model.Message) {
	a.messages = append(a.messages, m)
	if a.opts.SessLog != nil {
		_ = a.opts.SessLog.Append(m) // persistence errors surface on rebuild
	}
}

// Agent runs the loop.
type Agent struct {
	opts     Options
	messages []model.Message
	tokens   int
	turn     int
	budgetMu sync.Mutex
	reserved int // tokens reserved for running sub-agents

	lastCached         int
	lastCachedReported bool
	fatalGuard         bool // restore failure: stop the loop
	noticeMu           sync.Mutex
	notices            []model.Message // async event messages injected at next model call
	// lastSnap is the most recent trusted protected-set snapshot.
	lastSnap   guard.Snapshot
	prefixHash string // stable request prefix (system + tools)

	todo  *TodoTool
	loops *loopDetector
	// loopStopped marks a loop-detector termination for the log.
	loopStopped bool
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
	if opts.TodoDir != "" {
		a.todo = &TodoTool{Dir: opts.TodoDir}
	}
	a.loops = &loopDetector{cfg: opts.Loop}
	return a
}

// computePrefixHash hashes the deterministic request prefix:
// the system message followed by the tool schemas in registration
// order. Struct JSON marshalling is deterministic (fields in
// declaration order, no maps).
func (a *Agent) computePrefixHash() string {
	prefix := struct {
		System model.Message     `json:"system"`
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

// NewResultToolCall renders the synthetic (assistant tool_call, tool
// result) message pair used to fold an async sub-agent result into the
// main conversation. The tool result carries the data-not-instruction
// marker; both messages are appended to the persistent main-conversation
// log (core/sessionlog) at injection time so a resume rebuilds exactly
// what the model saw. The pair is delivered exactly once: delivery is
// recorded in the subagent audit events (delivered), and restarts
// re-inject only results that finished but were never delivered.
func NewResultToolCall(id, content string) (assistant, tool model.Message) {
	return model.Message{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{{
				ID:   "subnotice-" + id,
				Type: "function",
				Function: model.Function{
					Name:      "agent_result",
					Arguments: `{"id":"` + id + `"}`,
				},
			}},
		}, model.Message{
			Role:       model.RoleTool,
			Name:       "agent_result",
			ToolCallID: "subnotice-" + id,
			Content:    content,
		}
}

// PushResult queues one synthetic result pair for injection at the
// next model call. The caller (subagent.Manager) has already appended
// the same pair to the persistent main-conversation log.
func (a *Agent) PushResult(assistant, tool model.Message) {
	a.noticeMu.Lock()
	a.notices = append(a.notices, assistant, tool)
	a.noticeMu.Unlock()
}

// RestoreHistory appends a rebuilt message sequence (from
// sessionlog.RebuildWithWarning) to the in-memory history. It bypasses
// logMsg on purpose: these messages are already persisted, and writing
// them again would duplicate them in the log. System messages are
// skipped — New already installed the current system prompt.
func (a *Agent) RestoreHistory(msgs []model.Message) {
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			continue
		}
		a.messages = append(a.messages, m)
	}
}

// RestoreHistoryRepaired appends a rebuilt sequence and repairs
// dangling tool calls: an assistant message whose tool_calls never got
// a tool result (crash between the call and its result) is refused by
// providers when replayed. Each dangling call receives a synthetic
// tool result ("interrupted, outcome unknown"); the repairs are new
// records and go through logMsg, so they are also appended to the
// session log. Returns the number of repairs and a warning string.
func (a *Agent) RestoreHistoryRepaired(msgs []model.Message) (int, string) {
	a.RestoreHistory(msgs)
	answering := map[string]bool{}
	for _, m := range a.messages {
		if m.Role == model.RoleTool && m.ToolCallID != "" {
			answering[m.ToolCallID] = true
		}
	}
	var dangling []model.ToolCall
	for _, m := range a.messages {
		if m.Role != model.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !answering[tc.ID] {
				dangling = append(dangling, tc)
			}
		}
	}
	if len(dangling) == 0 {
		return 0, ""
	}
	for _, tc := range dangling {
		a.logMsg(model.Message{
			Role:       model.RoleTool,
			Name:       tc.Function.Name,
			ToolCallID: tc.ID,
			Content:    "tool execution was interrupted by a restart; outcome unknown",
		})
	}
	return len(dangling), fmt.Sprintf(
		"repaired %d dangling tool call(s) left by the crash", len(dangling))
}

// takeNotices drains queued notices.
func (a *Agent) takeNotices() []model.Message {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	out := a.notices
	a.notices = nil
	return out
}

// ReserveBudget atomically reserves n tokens from this agent's
// remaining budget; returns false when the remainder is insufficient.
// ReleaseBudget returns unused reservation on sub-agent completion.
func (a *Agent) ReserveBudget(n int) bool {
	a.budgetMu.Lock()
	defer a.budgetMu.Unlock()
	remain := a.maxTokens() - a.tokens - a.reserved
	if n > remain {
		return false
	}
	a.reserved += n
	return true
}

// ReleaseBudget returns unused reserved tokens.
func (a *Agent) ReleaseBudget(n int) {
	a.budgetMu.Lock()
	a.reserved -= n
	if a.reserved < 0 {
		a.reserved = 0
	}
	a.budgetMu.Unlock()
}

// RemainingTokens reports the unspent, unreserved budget.
func (a *Agent) RemainingTokens() int {
	a.budgetMu.Lock()
	defer a.budgetMu.Unlock()
	return a.maxTokens() - a.tokens - a.reserved
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

	a.logMsg(model.Message{Role: model.RoleUser, Content: userText})

	var (
		final      string
		toolRecs   []decision.ToolRecord
		runErr     error
		steps      int
		budgetStop string // non-empty when a budget cap forced the wrap-up
		turnPrompt int    // usage summed over this turn's model calls
		turnCompl  int
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
		turnPrompt += usage.PromptTokens
		turnCompl += usage.CompletionTokens
		a.lastCached = usage.CachedTokens
		a.lastCachedReported = usage.CachedReported
		if err != nil {
			runErr = err
			break
		}
		a.logMsg(msg)

		if len(msg.ToolCalls) == 0 {
			final = msg.Content
			break
		}

		// Execute tools in order, feed results back.
		loopStop := false
		for _, tc := range msg.ToolCalls {
			rec := a.execTool(ctx, tc)
			toolRecs = append(toolRecs, rec)
			// Loop detection on the last tool result.
			if a.loops != nil && len(a.messages) > 0 {
				last := a.messages[len(a.messages)-1]
				input := tc.Function.Arguments
				switch a.loops.observe(tc.Function.Name, input, last.Content) {
				case "remind":
					a.logMsg(model.Message{
						Role: model.RoleUser, Content: loopReminder,
					})
				case "stop":
					loopStop = true
				}
			}
		}
		if loopStop {
			final = a.wrapUp("loop detected: same tool call with same result repeated; stopping")
			runErr = fmt.Errorf("loop detected; run stopped (see decisions log)")
			a.loopStopped = true
			break
		}
	}

	a.logTurn(start, userText, toolRecs, len(toolRecs) == 0, runErr, budgetStop, turnPrompt, turnCompl)
	return final, runErr
}

// step makes one model call with the full toolset.
func (a *Agent) step(ctx context.Context) (model.Message, model.Usage, error) {
	req := model.Request{
		Model:    a.opts.Config.Model.Name,
		Messages: a.viewForModel(),
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

// stepNoTools makes one model call with the toolset stripped: used by
// wrap-up, where the only acceptable answer is plain text. The request
// prefix (system + tools) is unchanged for cache stability; tools are
// dropped from the wire payload only.
func (a *Agent) stepNoTools(ctx context.Context) (model.Message, model.Usage, error) {
	req := model.Request{
		Model:    a.opts.Config.Model.Name,
		Messages: a.viewForModel(),
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
			a.logMsg(model.Message{
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
	// Inline & spill: oversize results leave the context; errors keep
	// their tail (failures live at the end).
	if a.opts.Spill != nil {
		if inlined, serr := a.opts.Spill.Result(result); serr == nil {
			result = inlined
		}
	}

	a.logMsg(model.Message{
		Role: model.RoleTool, Name: tc.Function.Name,
		ToolCallID: tc.ID, Content: result,
	})
	return rec
}

// BudgetExhaustedSummary is the fixed final text when the wrap-up call
// cannot produce a usable summary (tool call or empty). It is never
// empty, so no delivery path can treat "" as a summary. Exported for
// the sub-agent manager's delivery guard.
const BudgetExhaustedSummary = "子代理在步数预算内未完成，没有摘要"

const budgetExhaustedSummary = BudgetExhaustedSummary

// wrapUp asks the model to finish within constraints. The wrap-up call
// carries no tools: the only acceptable answer is plain text. If the
// model still returns a tool call (or empty text), the call is NOT
// executed and the fixed budget-exhausted summary is returned instead —
// an empty string must never become a delivered summary.
func (a *Agent) wrapUp(reason string) string {
	a.logMsg(model.Message{
		Role: model.RoleUser,
		Content: fmt.Sprintf(
			"System: %s. Stop working and give a brief final summary of what was done.", reason),
	})
	msg, usage, err := a.stepNoTools(context.Background())
	a.tokens += usage.Total()
	if err != nil {
		return "(failed to produce summary: " + err.Error() + ")"
	}
	a.logMsg(msg)
	if len(msg.ToolCalls) > 0 || strings.TrimSpace(msg.Content) == "" {
		return budgetExhaustedSummary
	}
	return msg.Content
}

// logTurn writes the decision record. budgetStop names the cap that
// forced the wrap-up ("turn_limit" | "token_budget"), empty otherwise;
// it is recorded as an error-outcome so truncation is visible in the log.
func (a *Agent) logTurn(start time.Time, input string, tools []decision.ToolRecord, repliedDirectly bool, err error, budgetStop string, promptTokens, completionTokens int) {
	if a.opts.Decisions == nil {
		return
	}
	outcome := decision.OutcomeSuccess
	errMsg := ""
	switch {
	case a.loopStopped:
		outcome = decision.OutcomeError
		errMsg = "loop stop: repeated identical tool call (reminder ignored)"
	case err != nil:
		outcome = decision.OutcomeError
		errMsg = err.Error()
	case budgetStop != "":
		outcome = decision.OutcomeError
		errMsg = "budget stop: " + budgetStop + " (forced wrap-up)"
	}
	rec := decision.Record{
		SessionID:        a.opts.SessionID,
		Turn:             a.turn,
		InputDigest:      digest(input),
		Model:            a.opts.Config.Model.Name,
		ToolsCalled:      tools,
		RepliedDirectly:  repliedDirectly,
		DurationMS:       time.Since(start).Milliseconds(),
		Outcome:          outcome,
		Error:            errMsg,
		SchemaV2:         true,
		PrefixHash:       a.prefixHash,
		CachedTokens:     a.lastCached,
		CachedReported:   a.lastCachedReported,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
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
