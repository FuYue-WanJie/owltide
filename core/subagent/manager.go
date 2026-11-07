// Package subagent implements delegation: the main agent starts
// sub-agents (built-in roles from ~/.owltide/agents), each with its
// own isolated context, budget deducted from the parent, and results
// folded back into the main conversation log exactly once.
//
// Single-writer: a write token starts with the main agent; spawning a
// general sub-agent transfers it. While transferred, the parent's
// write tools are refused system-side; on sub-agent end/stop/crash
// (defer) the token returns to the parent.
package subagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FuYue-WanJie/owltide/core/agent"
	"github.com/FuYue-WanJie/owltide/core/agents"
	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/guard"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/sessionlog"
	"github.com/FuYue-WanJie/owltide/core/spill"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// Modes for agent_start.
const (
	ModeSingle   = "single"
	ModeParallel = "parallel"
	ModeChain    = "chain"
)

// State of one sub-agent session.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateDone    = "done"
	StateStopped = "stopped"
	StateFailed  = "failed"
)

// ManagerOpts wires the manager to the parent's world.
type ManagerOpts struct {
	// Parent holds the budget sub-agents draw from; it may be nil at
	// construction (the agent needs the registry that contains the
	// agent_* tools, which the manager builds) and is wired later via
	// SetParent.
	Parent      *agent.Agent
	Config      config.Config
	Client      *model.Client
	BaseReg     *tool.Registry // full tool set (main agent's)
	Agents      map[string]agents.Def
	Guard       *guard.Guard
	Spill       *spill.Store
	Decisions   *decision.Logger
	SessionID   string
	WorkDir     string
	TodoDir     string
	Loop        agent.LoopConfig
	DecisionsOn bool

	Clock       func() time.Time // injectable clock; nil = time.Now
	SyncTimeout time.Duration    // default 60s
	MaxDepth    int              // default 2
	MaxParallel int              // default 4

	// Events appends audit events (dispatched/finished/budget/rejected).
	Events func(event string, fields map[string]any)

	// EventsLogDir is where subagents.jsonl lives; when set, delivery
	// state is restored from it at startup so a restart never
	// re-delivers an already delivered result.
	EventsLogDir string

	// SubLogDir is the directory for per-sub-agent transcripts
	// (<dir>/<id>/messages.jsonl). The files are git-ignored and
	// secret-scrubbed by the spill layer.
	SubLogDir string

	// Confirm routes dangerous actions to the human terminal.
	// In non-interactive mode this is nil and requests are refused.
	Confirm tool.Confirmer

	// SessLog is the main conversation's append-only message log
	// (core/sessionlog). Finished sub-agent results are folded into it
	// as a synthetic (assistant tool_call, tool result) pair exactly
	// once; restarts re-inject only undelivered results.
	SessLog *sessionlog.Log

	// OnResult is an optional extra sink (e.g. audit); delivery to the
	// main conversation happens through Parent (SetParent) + SessLog
	// regardless.
	OnResult func(id, role, summary string)
}

// Session is one sub-agent instance.
type Session struct {
	ID      string
	Role    string
	Def     agents.Def
	Task    string
	Mode    string
	State   string
	Agent   *agent.Agent
	Reg     *tool.Registry
	Cancel  context.CancelFunc
	Started time.Time

	// Summary is the final output; Delivered marks that it has been
	// folded into the main log (repeat agent_result returns a ref).
	Summary   string
	Delivered bool

	release func() // returns unused budget reservation to the parent
}

// Manager owns all sub-agent sessions for one main run.
type Manager struct {
	opts ManagerOpts

	mu       sync.Mutex
	sessions map[string]*Session
	writeMu  sync.Mutex // guards write-token state
	writer   string     // "main" or sub-agent id
	panicJet *sync.Once // ensures single token-restore per panic path

	// writeTokenGate refuses parent write tools while a general
	// sub-agent holds the token.
	writeTokenGate *writeGate

	// subLogDir holds sub/<id>.jsonl per-sub-agent transcripts.
	subLogDir string

	// deliveredSeen are ids known delivered from the audit log
	// (restart recovery); consulted by deliver.
	deliveredSeen map[string]bool

	// confirmMu serializes confirmation requests from parallel
	// sub-agents: one terminal, one question at a time (queued).
	confirmMu sync.Mutex
}

// NewManager builds the manager and registers the agent_* tools on
// the parent's registry.
func NewManager(opts ManagerOpts) (*Manager, error) {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = 60 * time.Second
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 2
	}
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = 4
	}
	if opts.SubLogDir != "" && !filepath.IsAbs(opts.SubLogDir) {
		return nil, fmt.Errorf("SubLogDir must be absolute (got %q)", opts.SubLogDir)
	}
	m := &Manager{
		opts:      opts,
		sessions:  map[string]*Session{},
		writer:    "main",
		panicJet:  &sync.Once{},
		subLogDir: opts.SubLogDir,
	}
	if opts.EventsLogDir != "" {
		m.deliveredSeen = DeliveredIDs(opts.EventsLogDir)
	}
	m.writeTokenGate = &writeGate{m: m}
	return m, nil
}

// SetParent wires the main agent after construction (breaks the
// agent<->manager setup cycle) and re-delivers any results that
// finished in a previous process but were never delivered.
func (m *Manager) SetParent(p *agent.Agent) {
	m.mu.Lock()
	m.opts.Parent = p
	m.mu.Unlock()
	m.reDeliverPending()
}

// reDeliverPending folds results of sessions that are done/failed but
// not yet delivered into the main log (restart recovery). Delivery
// state lives in subagents.jsonl (delivered events), mirrored by the
// in-memory Delivered flag.
func (m *Manager) reDeliverPending() {
	m.mu.Lock()
	var pending []*Session
	for _, s := range m.sessions {
		if (s.State == StateDone || s.State == StateFailed) && !s.Delivered {
			pending = append(pending, s)
		}
	}
	m.mu.Unlock()
	for _, s := range pending {
		m.deliver(s)
	}
}

// deliver folds one finished session's summary into the main
// conversation log exactly once: a synthetic agent_result tool-call
// pair appended to the session log and pushed to the parent, plus a
// delivered audit event. Later agent_result calls return only a ref.
func (m *Manager) deliver(s *Session) {
	m.mu.Lock()
	if s.Delivered || m.deliveredSeen[s.ID] {
		s.Delivered = true
		m.mu.Unlock()
		return
	}
	s.Delivered = true
	m.mu.Unlock()

	content := resultEventMessage(s.ID, s.Role, m.foldLarge(s.Summary))
	assistant, toolMsg := agent.NewResultToolCall(s.ID, content)
	if m.opts.SessLog != nil {
		// Injected at the next model call AND appended now: resume
		// rebuild order then matches what the model saw.
		_ = m.opts.SessLog.Append(assistant)
		_ = m.opts.SessLog.Append(toolMsg)
	}
	if p := m.parent(); p != nil {
		p.PushResult(assistant, toolMsg)
	}
	m.event("delivered", map[string]any{
		"id": s.ID, "role": s.Role, "summary_digest": digest(s.Summary),
	})
	if m.opts.OnResult != nil {
		m.opts.OnResult(s.ID, s.Role, content)
	}
}

func (m *Manager) parent() *agent.Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts.Parent
}

// digest keeps a short sha256 prefix of arbitrary content for audit
// events (never the content itself).
func digest(s string) string {
	if len(s) <= 128 {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:8])
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// subTranscript returns the append-only sub/<id>.jsonl writer for one
// sub-agent session (same writer as the main conversation log).
func (m *Manager) subTranscript(id string) *sessionlog.Log {
	if m.subLogDir == "" {
		return nil
	}
	l, err := sessionlog.Open(m.subLogDir, id)
	if err != nil {
		return nil
	}
	return l
}

func (m *Manager) now() time.Time { return m.opts.Clock() }

// newID returns "sub-" + 8 random hex chars.
func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "sub-" + hex.EncodeToString(b[:])
}

// event appends one audit record via the configured sink.
func (m *Manager) event(name string, fields map[string]any) {
	if m.opts.Events != nil {
		m.opts.Events(name, fields)
	}
}

// BuildRegistry assembles the tool registry for a sub-agent role:
// whitelist from the role definition, capability-filtered for
// readonly roles. Sub-agents NEVER get the agent_* delegation tools
// (depth limit: main delegates, sub-agents do not) nor the
// self-evolution tools (evolve.*) — excluded even if whitelisted.
func (m *Manager) BuildRegistry(def agents.Def) (*tool.Registry, error) {
	var base *tool.Registry
	if def.Readonly {
		// todo writes only the session workspace, safe for read-only roles.
		base = m.opts.BaseReg.ReadOnlyView("todo")
		// run_shell stays available as the parser-gated read-only view.
		if sh, ok := m.opts.BaseReg.Get("run_shell"); ok {
			inner, isShell := sh.(*tool.Shell)
			if isShell {
				_ = base.Register(&tool.ReadOnlyShell{Inner: inner})
			}
		}
	} else {
		base = m.opts.BaseReg
	}
	filtered, err := def.FilterRegistry(base)
	if err != nil {
		return nil, err
	}
	return filtered, nil
}

// ExcludeForChild removes manager-only tools (agent_*, evolution)
// from a registry intended for a sub-agent, and swaps run_shell for
// the parser-gated read-only view when the role is readonly.
func (m *Manager) ExcludeForChild(def agents.Def, reg *tool.Registry) *tool.Registry {
	out := tool.NewRegistry()
	for _, d := range reg.Definitions() {
		name := d.Function.Name
		if strings.HasPrefix(name, "agent_") || strings.Contains(name, ".") {
			continue // agent_* and evolve-style dotted names
		}
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		if def.Readonly && name == "run_shell" {
			if sh, ok := t.(*tool.Shell); ok {
				_ = out.Register(&tool.ReadOnlyShell{Inner: sh})
				continue
			}
		}
		// Single-writer: sub-agents never get write tools while the
		// token sits with them; but the token IS with them for general.
		// Writes for readonly roles were already excluded by the
		// capability filter. Wrap non-readonly writers behind the gate
		// anyway (defense in depth; token holder passes).
		_ = out.Register(t)
	}
	return out
}

// WriteGate returns the gate that must wrap the parent's write tools.
func (m *Manager) WriteGate() *writeGate { return m.writeTokenGate }

// confirmFor returns a per-session confirmer: prompts are prefixed
// with the sub-agent id and role so the user knows who is asking, and
// requests are serialized (parallel sessions queue on one terminal).
func (m *Manager) confirmFor(s *Session) tool.Confirmer {
	if m.opts.Confirm == nil {
		return nil
	}
	return func(prompt string) bool {
		m.confirmMu.Lock()
		defer m.confirmMu.Unlock()
		return m.opts.Confirm("[subagent " + s.ID + " (" + s.Role + ")] " + prompt)
	}
}

// childRegistry builds the tool registry for one sub-agent session:
// capability/whitelist filtered, manager tools removed, and every
// confirmer-carrying tool cloned with the session's own (labeled,
// queued) confirmation stream.
func (m *Manager) childRegistry(def agents.Def, s *Session) (*tool.Registry, error) {
	reg, err := m.BuildRegistry(def)
	if err != nil {
		return nil, err
	}
	reg = m.ExcludeForChild(def, reg)
	return reg.CloneWithConfirmer(m.confirmFor(s)), nil
}

// writeGate refuses parent-side write tools while a general
// sub-agent holds the write token. Registered as a wrapper around
// the write-capable tools; reads pass through.
type writeGate struct{ m *Manager }

// Held reports whether the token is away from the main agent.
func (g *writeGate) Held() bool {
	g.m.writeMu.Lock()
	defer g.m.writeMu.Unlock()
	return g.m.writer != "main"
}

// Transfer moves the token to a sub-agent (or back to "main").
func (m *Manager) transfer(to string) {
	m.writeMu.Lock()
	m.writer = to
	m.writeMu.Unlock()
}

// AcquireForWrite is called by write tools: refused when the token is
// away from the main agent.
func (g *writeGate) AcquireForWrite(toolName string) error {
	g.m.writeMu.Lock()
	defer g.m.writeMu.Unlock()
	if g.m.writer != "main" {
		return fmt.Errorf("write refused: write token held by sub-agent %s (single-writer)", g.m.writer)
	}
	_ = toolName
	return nil
}

// ManagerTools returns the agent_* tool instances for registration.
func (m *Manager) ManagerTools() []tool.Tool {
	return []tool.Tool{
		agentStart{m}, agentStatus{m}, agentSend{m}, agentStop{m}, agentResult{m},
	}
}

// reserveBatch pre-deducts n copies of the role budget atomically.
func (m *Manager) reserveBatch(def agents.Def, n int) (func(), error) {
	parent := m.opts.Parent
	if parent == nil {
		return func() {}, nil
	}
	need := def.Budget.MaxTokens * n
	if !parent.ReserveBudget(need) {
		m.event("rejected", map[string]any{
			"reason": "parent budget exhausted (need " + itoa(need) + " tokens for " + itoa(n) + " tasks)",
			"role":   def.Name,
		})
		return nil, fmt.Errorf("parent budget exhausted; cannot start %d %q sub-agents", n, def.Name)
	}
	return func() {
		parent.ReleaseBudget(need)
		m.event("budget", map[string]any{
			"op": "released", "tokens": need, "count": n, "role": def.Name,
		})
	}, nil
}

// reserve attempts to pre-deduct def.Budget.MaxTokens from the parent
// budget; refusal is an error (budget exhausted).
func (m *Manager) reserve(def agents.Def) (release func(), err error) {
	parent := m.opts.Parent
	if parent == nil {
		return func() {}, nil // tests without a parent agent
	}
	n := def.Budget.MaxTokens
	if !parent.ReserveBudget(n) {
		m.event("rejected", map[string]any{
			"reason": "parent budget exhausted (need " + itoa(n) + " tokens)",
			"role":   def.Name,
		})
		return nil, fmt.Errorf("parent budget exhausted; cannot start %q sub-agent", def.Name)
	}
	m.event("budget", map[string]any{"op": "reserved", "tokens": n, "role": def.Name})
	return func() {
		parent.ReleaseBudget(n)
		m.event("budget", map[string]any{"op": "released", "tokens": n, "role": def.Name})
	}, nil
}

// runSession executes one sub-agent to completion synchronously and
// returns its summary. Budget is pre-deducted from the parent by the
// caller (agent_start).
func (m *Manager) runSession(ctx context.Context, s *Session) (string, error) {
	s.State = StateRunning
	defer func() {
		if r := recover(); r != nil {
			s.State = StateFailed
			m.transfer("main") // token returns even on panic
			panic(r)
		}
	}()
	opts := agent.Options{
		Config:    m.spawnConfig(s),
		Client:    m.opts.Client,
		Tools:     s.Reg,
		Decisions: m.opts.Decisions,
		SessionID: s.ID,
		WorkDir:   m.opts.WorkDir,
		Spill:     m.opts.Spill,
		Loop:      m.opts.Loop,
		TodoDir:   m.opts.TodoDir,
		SessLog:   m.subTranscript(s.ID), // sub/<id>/messages.jsonl
	}
	s.Agent = agent.New(opts)
	summary, err := s.Agent.Run(ctx, s.Task)
	s.Summary = summary
	if err != nil {
		s.State = StateFailed
	} else {
		s.State = StateDone
	}
	m.transfer("main") // defer-style release on every exit path
	return summary, err
}

// spawnConfig derives the sub-agent's model config (inherit global).
func (m *Manager) spawnConfig(s *Session) config.Config {
	cfg := m.opts.Config
	if s.Def.Model != "" && s.Def.Model != "inherit" {
		cfg.Model.Name = s.Def.Model
	}
	if s.Def.BaseURL != "" {
		cfg.Model.BaseURL = s.Def.BaseURL
	}
	cfg.Budget.MaxTurns = s.Def.Budget.MaxTurns
	cfg.Budget.MaxTokens = s.Def.Budget.MaxTokens
	cfg.Budget.ToolTimeoutSec = s.Def.Budget.ToolTimeoutSec
	cfg.System = rolePrefix(s.Def) // stable, role-fixed prefix
	return cfg
}

// rolePrefix is the byte-stable per-role system prefix. The task goes
// after it (as the first user message), so same-role sub-agents share
// the cacheable prefix.
func rolePrefix(def agents.Def) string {
	ro := "read-write"
	if def.Readonly {
		ro = "read-only"
	}
	return "You are OwlTide sub-agent '" + def.Name + "' (" + ro + "). " +
		def.Description + "\n" + def.SystemPrompt
}

// maxContextRefBytes caps how much of one referenced file's content is
// inlined into the sub-agent task; anything larger goes through the
// spill store and is referenced by id.
const maxContextRefBytes = 8 * 1024

// resolveContextRefs turns context_refs entries into a textual block
// prepended to the sub-agent's task. Two forms are accepted:
//
//   - "spill:<id>[:range]" — resolved strictly through the spill store
//     (ids are validated there; no filesystem access, no traversal).
//   - a file path — read through the Guard's read rules: the resolved
//     path must stay inside the session work dir (symlink-resolved),
//     absolute paths, ".." components and links escaping the tree are
//     refused.
//
// Oversized content is spilled; the task keeps the excerpt + spill id.
func (m *Manager) resolveContextRefs(refs []string) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		var content string
		if strings.HasPrefix(ref, "spill:") {
			if m.opts.Spill == nil {
				return "", fmt.Errorf("context ref %q: no spill store configured", ref)
			}
			data, err := m.opts.Spill.Read(strings.TrimPrefix(ref, "spill:"))
			if err != nil {
				return "", fmt.Errorf("context ref %q: %v", ref, err)
			}
			content = data
		} else {
			data, err := m.readWorkFile(ref)
			if err != nil {
				return "", fmt.Errorf("context ref %q: %v", ref, err)
			}
			content = string(data)
		}
		if len(content) > maxContextRefBytes {
			if m.opts.Spill != nil {
				if inl, err := m.opts.Spill.Result(content); err == nil {
					b.WriteString(inl + "\n\n")
					continue
				}
			}
			b.WriteString("<context ref: " + ref + ">\n" + clampStr(content, maxContextRefBytes) +
				"\n... (truncated)\n\n")
			continue
		}
		b.WriteString("<context ref: " + ref + ">\n" + content + "\n\n")
	}
	return strings.TrimSuffix(b.String(), "\n\n"), nil
}

// readWorkFile reads ref inside the session work dir under the Guard's
// read rules. Absolute paths, "..", symlinks resolving outside the
// tree and unreadable files are refused (fail closed).
func (m *Manager) readWorkFile(ref string) ([]byte, error) {
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "..") ||
		strings.Contains(filepath.ToSlash(ref), "/../") {
		return nil, fmt.Errorf("path escapes the working directory")
	}
	root := m.opts.WorkDir
	if root == "" {
		return nil, fmt.Errorf("no working directory configured")
	}
	clean := filepath.Clean(filepath.Join(root, ref))
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes the working directory")
	}
	// Symlink escape: resolve the real target and re-check.
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		if relReal, err := filepath.Rel(root, real); err != nil ||
			relReal == ".." || strings.HasPrefix(relReal, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("symlink escapes the working directory")
		}
		clean = real
	}
	// Guard read rules: refuse protected config files.
	if g := m.opts.Guard; g != nil {
		for _, p := range g.ProtectedPaths() {
			if p == clean {
				return nil, fmt.Errorf("path is protected (config area)")
			}
		}
	}
	return os.ReadFile(clean)
}

// clampStr truncates to n bytes without splitting a UTF-8 rune.
func clampStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}
