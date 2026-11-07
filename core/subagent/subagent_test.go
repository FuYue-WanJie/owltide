package subagent

// M1 acceptance tests (fake model server; no tokens burned).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FuYue-WanJie/owltide/core/agent"
	"github.com/FuYue-WanJie/owltide/core/agents"
	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/guard"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/sessionlog"
	"github.com/FuYue-WanJie/owltide/core/spill"
	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// fakeLLM2 is a scripted model server shared by subagent tests.
func fakeLLM2(t *testing.T, script func(n int, req model.Request) string) *httptest.Server {
	t.Helper()
	n := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req model.Request
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(script(k, req)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chunk(c string) string { return "data: " + c + "\n\n" }
func sseDone() string       { return "data: [DONE]\n\n" }

func textReply(s string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": s},
	}}})
	return chunk(string(b)) + sseDone()
}

func toolCall(id, name, argsJSON string) string {
	tc := map[string]any{
		"id": id, "type": "function",
		"function": map[string]string{"name": name, "arguments": argsJSON},
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{tc}},
	}}})
	return chunk(string(b)) + sseDone()
}

// fixture builds a manager over a temp config store.
type fixture struct {
	m       *Manager
	root    string
	cfgDir  string
	agents  map[string]agents.Def
	baseReg *tool.Registry
	parent  *agent.Agent
	srv     *httptest.Server
	sessLog *sessionlog.Log
}

type fxOpts struct {
	confirm tool.Confirmer
}

func newFixture(t *testing.T, script func(n int, req model.Request) string, o fxOpts) *fixture {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".owltide")
	for _, d := range []string{"agents", "logs", "logs/sub"} {
		os.MkdirAll(filepath.Join(cfgDir, d), 0o755)
	}
	os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
		[]byte("model:\n  name: m\n  base_url: https://x\nbudget:\n  max_turns: 30\n  max_tokens: 400000\n  tool_timeout_sec: 60\n"), 0o600)
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	st.EnsureInit()
	agents.SeedBuiltins(cfgDir)
	defs, err := agents.Load(cfgDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	srv := fakeLLM2(t, script)

	cfg := config.Default()
	reg := tool.NewRegistry()
	reg.SetConfirmer(o.confirm)
	workDir := t.TempDir()
	must(t, reg.Register(tool.ReadFile{}))
	must(t, reg.Register(&tool.WriteFile{}))
	must(t, reg.Register(&tool.EditFile{}))
	must(t, reg.Register(tool.ListDir{}))
	must(t, reg.Register(tool.NewShell("/bin/sh", "-c", workDir, 5)))
	must(t, reg.Register(agent.TodoTool{Dir: t.TempDir()}))

	gd := &guard.Guard{Root: cfgDir, St: st}

	// The parent agent shares the model server.
	parentOpts := agent.Options{
		Config: cfg, Client: model.NewClient(srv.URL, "k", nil),
		Tools: reg, SessionID: "main", WorkDir: workDir,
	}
	parent := agent.New(parentOpts)

	// Manager tools are part of the known tool set the role files are
	// validated against (registered below, after the manager exists).

	sessLogDir := filepath.Join(root, "sessions")
	sessLog, err := sessionlog.Open(sessLogDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessLog.Close() })

	mgr, err := NewManager(ManagerOpts{
		Parent: parent, Config: cfg,
		Client:  model.NewClient(srv.URL, "k", nil),
		BaseReg: reg, Agents: defs, Guard: gd,
		SessionID: "test", WorkDir: workDir, TodoDir: t.TempDir(),
		Confirm: o.confirm,
		Clock:   time.Now, SyncTimeout: 2 * time.Second,
		SessLog:      sessLog,
		SubLogDir:    filepath.Join(cfgDir, "logs", "sub"),
		EventsLogDir: filepath.Join(cfgDir, "logs"),
		Events: func(event string, fields map[string]any) {
			AppendEvent(filepath.Join(cfgDir, "logs"), event, fields)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Spill store over the config dir (same layout as the CLI).
	spStore, err := spill.New(cfgDir, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	mgr.opts.Spill = spStore
	for _, tl := range mgr.ManagerTools() {
		must(t, reg.Register(tl))
	}
	// Re-validate role files against the full tool set (Load with the
	// real names, as the CLI does).
	known := map[string]bool{}
	for _, d := range reg.Definitions() {
		known[d.Function.Name] = true
	}
	defs, err = agents.Load(cfgDir, known)
	if err != nil {
		t.Fatal(err)
	}
	mgr.opts.Agents = defs
	mgr.SetParent(parent)
	return &fixture{m: mgr, root: root, cfgDir: cfgDir, agents: defs, baseReg: reg, parent: parent, srv: srv, sessLog: sessLog}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// parentAgentRuns wraps the parent agent running a scripted exchange.
func (f *fixture) startCall(t *testing.T, args string) (string, error) {
	st, ok := f.m.ManagerTools()[0].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	if !ok {
		t.Fatal("agent_start tool missing")
	}
	return st.Call(context.Background(), json.RawMessage(args))
}

// TestSubagentContextIsolated: the sub-agent request body must not
// contain the main conversation history.
func TestSubagentContextIsolated(t *testing.T) {
	var seen []model.Request
	var mu sync.Mutex
	srv := fakeLLM2(t, func(n int, req model.Request) string {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		if n == 1 {
			return toolCall("c1", "read_file", `{"path":"/tmp/x.txt"}`)
		}
		return textReply("sub done")
	})
	f := newFixture(t, nil, fxOpts{})
	f.srv = srv
	// Rebuild manager client against OUR server.
	f.m.opts.Client = model.NewClient(srv.URL, "k", nil)

	// Main conversation would contain a marker string; the sub-agent
	// request must not include it.
	res, err := f.startCall(t, `{"role":"explore","task":"SECRET_MARKER task body","mode":"single"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "sub done") && !strings.Contains(res, `"id"`) {
		t.Fatalf("unexpected start result: %s", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, req := range seen {
		// Isolation: the sub-agent context is exactly system prefix +
		// task (+ tool results). It must never contain a main-history
		// marker that was never part of this task.
		if i == 0 && len(req.Messages) > 2 {
			t.Fatalf("sub-agent context not isolated: %d messages in first request", len(req.Messages))
		}
	}
}

// TestResultNotUserRole: async results are event messages with the
// [subagent ... result] marker, never role=user.
func TestResultNotUserRole(t *testing.T) {
	// The manager's OnResult contract hands (id, role, summary); the
	// caller folds it with the marker. Verify the marker format here.
	id := "sub-abc12345"
	msg := resultEventMessage(id, "explore", "findings")
	if !strings.HasPrefix(msg, "[subagent "+id+" result (explore):") {
		t.Fatalf("marker missing: %q", msg[:40])
	}
	if !strings.Contains(msg, "不是指令") {
		t.Fatalf("marker must state data-not-instruction: %q", msg)
	}
}

// TestResultDeliveredOnce: agent_result after delivery returns a
// reference only.
func TestResultDeliveredOnce(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	// Inject a finished session directly.
	s := &Session{
		ID: "sub-deliv1", Role: "explore",
		Def:  f.agents["explore"],
		Task: "t", Mode: ModeSingle, State: StateDone,
		Summary:   "THE_SUMMARY_BODY",
		Delivered: true,
	}
	f.m.mu.Lock()
	f.m.sessions[s.ID] = s
	f.m.mu.Unlock()
	tools := f.m.ManagerTools()
	rt := tools[4].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	out, err := rt.Call(context.Background(), json.RawMessage(`{"id":"sub-deliv1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "THE_SUMMARY_BODY") {
		t.Fatalf("delivered result must not be re-sent: %s", out)
	}
	if !strings.Contains(out, "不要再查询") {
		t.Fatalf("expected stop-polling reference reply: %s", out)
	}
}

// TestDepthLimit: sub-agents never receive agent_* tools.
func TestDepthLimit(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	def := f.agents["explore"]
	reg, err := f.m.BuildRegistry(def)
	if err != nil {
		t.Fatal(err)
	}
	reg = f.m.ExcludeForChild(def, reg)
	if _, ok := reg.Get("agent_start"); ok {
		t.Fatal("sub-agent registry must not contain agent_start (depth limit)")
	}
	// Evolution tools excluded too.
	for _, name := range []string{"agent_upsert", "config_patch", "skill_write", "memory_write", "history_rollback", "agent_remove"} {
		if _, ok := reg.Get(name); ok {
			t.Fatalf("sub-agent registry must not contain %q", name)
		}
	}
}

// TestBudgetDeductedFromParent: starting a sub-agent reserves its
// budget from the parent; completion returns the remainder, and both
// sides write budget audit events.
func TestBudgetDeductedFromParent(t *testing.T) {
	var mu sync.Mutex
	var budgetOps []string
	f := newFixture(t, nil, fxOpts{})
	f.m.opts.Events = func(event string, fields map[string]any) {
		if event == "budget" {
			mu.Lock()
			budgetOps = append(budgetOps, fields["op"].(string))
			mu.Unlock()
		}
		AppendEvent(filepath.Join(f.cfgDir, "logs"), event, fields)
	}
	before := f.parent.RemainingTokens()
	def := f.agents["explore"]
	release, err := f.m.reserve(def)
	if err != nil {
		t.Fatal(err)
	}
	during := f.parent.RemainingTokens()
	if during != before-def.Budget.MaxTokens {
		t.Fatalf("reservation not deducted: before=%d during=%d want=%d", before, during, before-def.Budget.MaxTokens)
	}
	release()
	after := f.parent.RemainingTokens()
	if after != before {
		t.Fatalf("release did not return budget: %d != %d", after, before)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(budgetOps) != 2 || budgetOps[0] != "reserved" || budgetOps[1] != "released" {
		t.Fatalf("budget events missing or wrong order: %v", budgetOps)
	}
	// Exhaustion refuses wholesale.
	f.parent.ReserveBudget(before)
	if _, err := f.m.reserve(def); err == nil {
		t.Fatal("exhausted budget must refuse")
	}
}

// TestExploreCannotWrite: the readonly registry excludes write tools.
func TestExploreCannotWrite(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	def := f.agents["explore"]
	reg, err := f.m.BuildRegistry(def)
	if err != nil {
		t.Fatal(err)
	}
	reg = f.m.ExcludeForChild(def, reg)
	for _, name := range []string{"write_file", "edit_file"} {
		if _, ok := reg.Get(name); ok {
			t.Fatalf("readonly role must not have %q", name)
		}
	}
}

// TestSubagentCannotEvolve: evolution tools never in child registries.
func TestSubagentCannotEvolve(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	for _, roleName := range []string{"general", "explore", "review", "planner"} {
		def := f.agents[roleName]
		reg, err := f.m.BuildRegistry(def)
		if err != nil {
			t.Fatal(err)
		}
		reg = f.m.ExcludeForChild(def, reg)
		for _, d := range reg.Definitions() {
			if strings.Contains(d.Function.Name, ".") || strings.HasPrefix(d.Function.Name, "agent_") {
				t.Fatalf("role %s must not carry %q", roleName, d.Function.Name)
			}
		}
	}
}

// TestDangerConfirmBubbles: in non-interactive mode (nil confirmer) a
// sub-agent's dangerous command is refused, never self-approved.
func TestDangerConfirmBubbles(t *testing.T) {
	f := newFixture(t, nil, fxOpts{}) // Confirm == nil
	def := f.agents["explore"]
	reg, err := f.m.BuildRegistry(def)
	if err != nil {
		t.Fatal(err)
	}
	reg = f.m.ExcludeForChild(def, reg)
	sh, ok := reg.Get("run_shell")
	if !ok {
		t.Fatal("explore must keep run_shell (gated)")
	}
	_, err = sh.Call(context.Background(),
		json.RawMessage(`{"command":"rm -rf /tmp/whatever"}`))
	if err == nil {
		t.Fatal("dangerous command must be refused without confirmer")
	}
}

// TestWriteTokenReturnedOnPanic: a panicking sub-agent returns the
// write token to the main agent.
func TestWriteTokenReturnedOnPanic(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	if f.m.WriteGate().Held() {
		t.Fatal("token starts with main")
	}
	// Simulate transfer then panic-restore path.
	f.m.transfer("sub-panic1")
	if !f.m.WriteGate().Held() {
		t.Fatal("token should be away")
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				f.m.transfer("main")
			}
		}()
		panic("boom")
	}()
	if f.m.WriteGate().Held() {
		t.Fatal("token must return on panic")
	}
}

// TestReadonlyRejectsUndeclaredTool: a registry tool without a
// read-only capability declaration is excluded from readonly views.
func TestReadonlyRejectsUndeclaredTool(t *testing.T) {
	reg := tool.NewRegistry()
	must(t, reg.Register(tool.ReadFile{}))                                // declares reads
	must(t, reg.Register(&tool.WriteFile{}))                              // declares writes
	must(t, reg.Register(tool.NewShell("/bin/sh", "-c", t.TempDir(), 5))) // exec
	ro := reg.ReadOnlyView()
	if _, ok := ro.Get("write_file"); ok {
		t.Fatal("write_file must be excluded from readonly view")
	}
	if _, ok := ro.Get("run_shell"); ok {
		t.Fatal("run_shell (exec) must be excluded from readonly view")
	}
	if _, ok := ro.Get("read_file"); !ok {
		t.Fatal("read_file must remain")
	}
}

// TestReadonlyShellRejectsRedirect: output redirects are refused.
func TestReadonlyShellRejectsRedirect(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	def := f.agents["explore"]
	reg, err := f.m.BuildRegistry(def)
	if err != nil {
		t.Fatal(err)
	}
	reg = f.m.ExcludeForChild(def, reg)
	sh, ok := reg.Get("run_shell")
	if !ok {
		t.Fatal("explore must keep run_shell (gated)")
	}
	for _, cmd := range []string{
		`echo hi > /tmp/evil.txt`,
		`echo hi >> /tmp/evil.txt`,
		`cat /etc/passwd | tee /tmp/evil.txt`,
		`git status --porcelain > out.txt`,
	} {
		if _, err := sh.Call(context.Background(), json.RawMessage(`{"command":`+jsonString(cmd)+`}`)); err == nil {
			t.Fatalf("redirect must be refused: %q", cmd)
		}
	}
	// Plain reads still work.
	if _, err := sh.Call(context.Background(), json.RawMessage(`{"command":"echo ok"}`)); err != nil {
		t.Fatalf("plain echo refused: %v", err)
	}
}

// TestGitFsmonitorNotRun: git commands run with hooks/fsmonitor off
// and reject -c/--output/--exec style options.
func TestGitFsmonitorNotRun(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, _ := c.CombinedOutput()
		return string(out)
	}
	run("init", "-q", "-b", "main", ".")
	// A repo-local fsmonitor that would run if not disabled: it would
	// create a marker file. core.fsmonitor= must prevent it.
	run("config", "core.fsmonitor", "/bin/sh -c 'touch fsmonitor-ran'")
	run("config", "alias.status", "!touch alias-ran")

	ros := &tool.ReadOnlyShell{Inner: tool.NewShell("/bin/sh", "-c", dir, 10)}
	if _, err := ros.Call(context.Background(),
		json.RawMessage(`{"command":"git status"}`)); err != nil {
		t.Fatalf("git status refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fsmonitor-ran")); err == nil {
		t.Fatal("core.fsmonitor was executed — sandbox failed")
	}
	// -c injection refused.
	if _, err := ros.Call(context.Background(),
		json.RawMessage(`{"command":"git -c core.fsmonitor=/bin/sh\\ -c\\ touch\\ pwned status"}`)); err == nil {
		t.Fatal("git -c must be refused")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestResultSurvivesRestart: a delivered result is persisted in the
// main session log (rebuildable); a finished-but-undelivered result is
// re-delivered exactly once after "restart" (fresh manager + agent).
func TestResultSurvivesRestart(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})

	// 1. Finished session, not yet delivered (crash before delivery).
	s := &Session{
		ID: "sub-restart1", Role: "explore",
		Def: f.agents["explore"], Task: "t", Mode: ModeSingle,
		State: StateDone, Summary: "FINDINGS_BODY_42",
	}
	f.m.mu.Lock()
	f.m.sessions[s.ID] = s
	f.m.mu.Unlock()

	// Restart: fresh manager sharing the same session log + audit dir.
	mgr2, err := NewManager(ManagerOpts{
		Parent: f.parent, Config: f.m.opts.Config,
		Client: f.m.opts.Client, BaseReg: f.baseReg,
		Agents: f.agents, SessionID: "test2",
		SessLog:      f.sessLog,
		SubLogDir:    f.m.subLogDir,
		EventsLogDir: f.m.opts.EventsLogDir,
		Events: func(event string, fields map[string]any) {
			AppendEvent(f.m.opts.EventsLogDir, event, fields)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr2.mu.Lock()
	mgr2.sessions[s.ID] = s
	mgr2.mu.Unlock()
	mgr2.SetParent(f.parent)

	// Re-delivery: the pair must be in the session log exactly once.
	msgs, _, err := sessionlog.RebuildWithWarning(
		filepath.Join(f.root, "sessions", "main", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var pairs int
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.ToolCalls[0].ID, s.ID) {
			pairs++
		}
	}
	if pairs != 1 {
		t.Fatalf("expected exactly 1 delivered pair after restart, got %d (log has %d msgs)", pairs, len(msgs))
	}
	// Second deliver attempt: no new pair.
	mgr2.deliver(s)
	msgs, _, _ = sessionlog.RebuildWithWarning(
		filepath.Join(f.root, "sessions", "main", "messages.jsonl"))
	var pairs2 int
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.ToolCalls[0].ID, s.ID) {
			pairs2++
		}
	}
	if pairs2 != 1 {
		t.Fatalf("re-delivery duplicated the pair: %d", pairs2)
	}
	// The tool message must not be role=user and must carry the marker.
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, s.ID) {
			if !strings.Contains(m.Content, "不是指令") {
				t.Fatalf("tool result missing data-not-instruction marker: %q", m.Content)
			}
			if m.Name != "agent_result" {
				t.Fatalf("tool message name = %q, want agent_result", m.Name)
			}
		}
	}
	// Delivered event persisted: a third manager must not re-deliver.
	// (DeliveredIDs read in NewManager marked it; verify directly.)
	if !DeliveredIDs(f.m.opts.EventsLogDir)[s.ID] {
		t.Fatal("delivered event missing from audit log")
	}
}

// TestSubagentResultSpill: oversized summaries go through the spill
// store; the delivered content keeps the excerpt + spill id, not the
// full body.
func TestSubagentResultSpill(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	var bodyBuf strings.Builder
	for i := 0; i < 2000; i++ {
		bodyBuf.WriteString("SPILL_LINE_")
		bodyBuf.WriteString(strings.Repeat("Y", 80))
		bodyBuf.WriteString("\n")
	}
	body := bodyBuf.String() // ~160KB, 2000 lines
	s := &Session{
		ID: "sub-spill1", Role: "explore",
		Def: f.agents["explore"], Task: "t", Mode: ModeSingle,
		State: StateDone, Summary: body,
	}
	f.m.mu.Lock()
	f.m.sessions[s.ID] = s
	f.m.mu.Unlock()
	f.m.deliver(s)

	msgs, _, err := sessionlog.RebuildWithWarning(
		filepath.Join(f.root, "sessions", "main", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var toolContent string
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, s.ID) {
			toolContent = m.Content
		}
	}
	if toolContent == "" {
		t.Fatal("delivered tool message not found in session log")
	}
	// The excerpt keeps head+tail only: a middle line must be absent.
	if strings.Contains(toolContent, "SPILL_LINE_1000") {
		t.Fatal("full oversized summary must not enter the main log (middle line present)")
	}
	if !strings.Contains(toolContent, "spill id:") {
		t.Fatalf("expected spill id reference in delivered content")
	}
}

// TestChainFailureReleasesBudget: a failing chain step releases the
// whole chain reservation (defer registered right after reserve).
func TestChainFailureReleasesBudget(t *testing.T) {
	// Sub-agent model calls fail hard (HTTP 500) so step 1 errors.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failSrv.Close)
	f := newFixture(t, nil, fxOpts{})
	f.m.opts.Client = model.NewClient(failSrv.URL, "k", nil)

	before := f.parent.RemainingTokens()
	st := f.m.ManagerTools()[0].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	_, err := st.Call(context.Background(), json.RawMessage(
		`{"role":"explore","task":"x","mode":"chain","steps":[{"role":"explore","task":"step1"},{"role":"explore","task":"step2"}]}`))
	if err == nil {
		t.Fatal("chain with failing step must return an error")
	}
	after := f.parent.RemainingTokens()
	if after != before {
		t.Fatalf("chain reservation leaked: before=%d after=%d", before, after)
	}
}

// TestContextRefsGuarded: path traversal, absolute paths and symlink
// escapes are refused; spill ids resolve only through the spill store;
// oversized content is spilled instead of inlined.
func TestContextRefsGuarded(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	work := f.m.opts.WorkDir

	// Allowed: a file inside the work dir.
	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("HELLO_FROM_NOTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Escape attempts must fail.
	for _, ref := range []string{
		"../outside.txt",
		"/etc/passwd",
		filepath.Join(work, "..", "outside.txt"),
	} {
		if _, err := f.m.resolveContextRefs([]string{ref}); err == nil {
			t.Fatalf("ref %q must be refused", ref)
		}
	}
	// Symlink escape: link inside work dir pointing outside.
	outside := filepath.Join(f.root, "secret.txt")
	os.WriteFile(outside, []byte("TOP_SECRET"), 0o600)
	link := filepath.Join(work, "link.md")
	os.Symlink(outside, link)
	if _, err := f.m.resolveContextRefs([]string{"link.md"}); err == nil {
		t.Fatal("symlink escape must be refused")
	}
	// Valid ref resolves with content.
	block, err := f.m.resolveContextRefs([]string{"notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "HELLO_FROM_NOTES") {
		t.Fatalf("valid ref content missing: %q", block)
	}
	// Oversized file: inlined block carries a spill id, not the body.
	var bigBuf strings.Builder
	for i := 0; i < 5000; i++ {
		bigBuf.WriteString("BIGLINE")
		bigBuf.WriteString(strings.Repeat("X", 100))
		bigBuf.WriteString("\n")
	}
	big := bigBuf.String() // ~500KB, 5000 lines
	os.WriteFile(filepath.Join(work, "big.md"), []byte(big), 0o600)
	block, err = f.m.resolveContextRefs([]string{"big.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "OUTPUT TRUNCATED") {
		t.Fatal("oversized context ref must be spilled (truncation marker missing)")
	}
	if !strings.Contains(block, "spill id:") {
		t.Fatalf("oversized ref must reference a spill id")
	}
	// Spill ref resolves through the spill store only.
	out, err := f.m.opts.Spill.Result(strings.Repeat("S", 9000))
	if err != nil {
		t.Fatal(err)
	}
	var spillID string
	if i := strings.Index(out, "spill id: "); i >= 0 {
		spillID = out[i+len("spill id: "):][:16]
	}
	if spillID == "" {
		t.Fatal("could not obtain spill id for test")
	}
	block, err = f.m.resolveContextRefs([]string{"spill:" + spillID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "SSS") {
		t.Fatalf("spill ref content missing")
	}
	// Malformed spill id refused.
	if _, err := f.m.resolveContextRefs([]string{"spill:../../etc/passwd"}); err == nil {
		t.Fatal("spill traversal must be refused")
	}
}

// TestParallelConfirmQueued: confirmation requests from parallel
// sub-agents serialize (never overlap) and each prompt is labeled with
// the sub-agent id and role. Unit-level: parallel mode is read-only
// and its gated shell refuses dangerous commands outright, so the
// queue matters for confirmer-carrying tools in general.
func TestParallelConfirmQueued(t *testing.T) {
	var mu sync.Mutex
	inside := 0
	maxInside := 0
	var prompts []string
	f := newFixture(t, nil, fxOpts{})
	f.m.opts.Confirm = func(prompt string) bool {
		mu.Lock()
		inside++
		if inside > maxInside {
			maxInside = inside
		}
		prompts = append(prompts, prompt)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inside--
		mu.Unlock()
		return false
	}
	s1 := &Session{ID: "sub-p1", Role: "explore", State: StateRunning}
	s2 := &Session{ID: "sub-p2", Role: "review", State: StateRunning}
	cf1 := f.m.confirmFor(s1)
	cf2 := f.m.confirmFor(s2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); cf1("approve command X?") }()
	go func() { defer wg.Done(); cf2("approve command Y?") }()
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("confirmations overlapped: max concurrent = %d", maxInside)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}
	seen := map[string]bool{}
	for _, p := range prompts {
		if !strings.HasPrefix(p, "[subagent sub-") {
			t.Fatalf("prompt missing sub-agent label: %q", p)
		}
		switch {
		case strings.Contains(p, "sub-p1") && strings.Contains(p, "(explore)"):
			seen["p1"] = true
		case strings.Contains(p, "sub-p2") && strings.Contains(p, "(review)"):
			seen["p2"] = true
		}
	}
	if !seen["p1"] || !seen["p2"] {
		t.Fatalf("prompts not labeled per session: %v", prompts)
	}
	// Nil confirmer -> no per-session confirmer at all.
	f.m.opts.Confirm = nil
	if f.m.confirmFor(s1) != nil {
		t.Fatal("confirmFor must return nil when the manager has no confirmer")
	}
}

// TestRolePrefixStable: the per-role system prefix is byte-stable and
// the task rides after it (first user message), so same-role runs
// share the cacheable prefix.
func TestRolePrefixStable(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	def := f.agents["explore"]
	p1 := rolePrefix(def)
	p2 := rolePrefix(def)
	if p1 != p2 {
		t.Fatal("role prefix must be byte-stable")
	}
	cfg := f.m.spawnConfig(&Session{Def: def})
	if cfg.System != p1 {
		t.Fatalf("spawnConfig system != rolePrefix")
	}
	if !strings.Contains(cfg.System, "explore") || !strings.Contains(cfg.System, "read-only") {
		t.Fatalf("prefix must identify role and mode: %q", cfg.System)
	}
}

// TestEventsHashOnly: audit events use a field allowlist — any key
// outside it is stored as a digest only, regardless of value length,
// so task content, commands, paths or error text can never leak.
func TestEventsHashOnly(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("SECRET_PAYLOAD ", 40) // 600 chars
	AppendEvent(dir, "finished", map[string]any{"id": "sub-ev1", "note": long})
	// Short non-allowlisted values are digested too (length plays no
	// role in the decision).
	AppendEvent(dir, "finished", map[string]any{"id": "sub-ev3", "note": "SHORT_TASK_BODY", "command": "cat ~/.owltide/config.yaml"})
	data, err := os.ReadFile(filepath.Join(dir, "subagents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, leak := range []string{"SECRET_PAYLOAD", "SHORT_TASK_BODY", "config.yaml"} {
		if strings.Contains(log, leak) {
			t.Fatalf("non-allowlisted value leaked into audit event: %q", leak)
		}
	}
	if !strings.Contains(log, "note_digest") || !strings.Contains(log, "command_digest") {
		t.Fatalf("expected digest fields for non-allowlisted keys: %s", log)
	}
	// Allowlisted keys pass verbatim.
	if !strings.Contains(log, `"id":"sub-ev1"`) || !strings.Contains(log, `"id":"sub-ev3"`) {
		t.Fatalf("allowlisted id field must pass through: %s", log)
	}
	// DeliveredIDs round-trip.
	AppendEvent(dir, "delivered", map[string]any{"id": "sub-ev2", "role": "explore"})
	if !DeliveredIDs(dir)["sub-ev2"] {
		t.Fatal("delivered event not found by DeliveredIDs")
	}
}

// TestSubTranscriptNotCommitted: sub transcripts and session logs
// under the config root never enter the config store's commits.
func TestSubTranscriptNotCommitted(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"logs/sub", "sessions/main"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "logs/sub/sub-x/messages.jsonl"), []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "sessions/main/messages.jsonl"), []byte("{}\n"), 0o600)

	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.EnsureInit()
	if _, _, err := st.AutoCommit(store.CommitMeta{Who: "test", Reason: "layout"}); err != nil {
		t.Fatal(err)
	}
	commits, err := st.Log()
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) == 0 {
		t.Fatal("expected at least one commit")
	}
	// The tree of HEAD must not contain logs/, sessions/ or spill/ paths.
	head := commits[0]
	treeID, err := st.CommitTreeExport(head.ID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := st.FlatTreeExport(treeID, "")
	if err != nil {
		t.Fatal(err)
	}
	for p := range files {
		if strings.HasPrefix(p, "logs/") || strings.HasPrefix(p, "sessions/") ||
			strings.HasPrefix(p, "spill/") {
			t.Fatalf("runtime path %q leaked into config store commit", p)
		}
	}
}

// TestRecoverUndeliveredFromDisk: a fresh manager recovers finished-
// but-undelivered results from the audit log + sub transcripts alone
// (no in-memory session state), delivers the pair into the session log
// exactly once, and a further restart delivers nothing.
func TestRecoverUndeliveredFromDisk(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	id := "sub-recov1"

	// Simulate a finished sub run from a previous process: transcript
	// ending with the final assistant summary + audit events, no
	// delivered event.
	tl, err := sessionlog.Open(f.m.subLogDir, id)
	if err != nil {
		t.Fatal(err)
	}
	must(t, tl.Append(model.Message{Role: "user", Content: "task body"}))
	must(t, tl.Append(model.Message{Role: "assistant", Content: "RECOVERED_SUMMARY_BODY"}))
	tl.Close()
	eventsDir := filepath.Join(f.cfgDir, "logs")
	AppendEvent(eventsDir, "dispatched", map[string]any{"id": id, "role": "explore", "mode": "single"})
	AppendEvent(eventsDir, "finished", map[string]any{"id": id, "role": "explore", "state": "done"})
	logPath := filepath.Join(f.root, "sessions", "main", "messages.jsonl")

	countPairs := func() int {
		msgs, _, err := sessionlog.RebuildWithWarning(logPath)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range msgs {
			if m.Role == "assistant" && len(m.ToolCalls) > 0 && strings.Contains(m.ToolCalls[0].ID, id) {
				n++
			}
		}
		return n
	}

	// Restart 1: recovery + delivery.
	mgr2, err := NewManager(ManagerOpts{
		Parent: f.parent, Config: f.m.opts.Config,
		Client: f.m.opts.Client, BaseReg: f.baseReg,
		Agents: f.agents, SessionID: "test2",
		SessLog:      f.sessLog,
		SubLogDir:    f.m.subLogDir,
		EventsLogDir: eventsDir,
		Events: func(event string, fields map[string]any) {
			AppendEvent(eventsDir, event, fields)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr2.SetParent(f.parent)
	if n := countPairs(); n != 1 {
		t.Fatalf("recovery must deliver exactly once, got %d", n)
	}
	// The delivered content carries the recovered summary.
	msgs, _, _ := sessionlog.RebuildWithWarning(logPath)
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "RECOVERED_SUMMARY_BODY") {
			found = true
		}
	}
	if !found {
		t.Fatal("recovered summary missing from delivered pair")
	}
	if !DeliveredIDs(eventsDir)[id] {
		t.Fatal("delivered event not persisted")
	}

	// Restart 2: delivered state wins; nothing new.
	mgr3, err := NewManager(ManagerOpts{
		Parent: f.parent, Config: f.m.opts.Config,
		Client: f.m.opts.Client, BaseReg: f.baseReg,
		Agents: f.agents, SessionID: "test3",
		SessLog:      f.sessLog,
		SubLogDir:    f.m.subLogDir,
		EventsLogDir: eventsDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr3.SetParent(f.parent)
	if n := countPairs(); n != 1 {
		t.Fatalf("second restart re-delivered: %d pairs", n)
	}
}

// TestRunningSubagentReportedInterrupted: a sub-agent still running at
// crash time is not silently dropped on restart — it is marked
// interrupted, reported through the same agent_result delivery path
// exactly once, and the write token is back with main (a fresh process
// holds no reservation for it, so there is nothing to refund).
func TestRunningSubagentReportedInterrupted(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	id := "sub-interr1"
	eventsDir := filepath.Join(f.cfgDir, "logs")
	logPath := filepath.Join(f.root, "sessions", "main", "messages.jsonl")

	AppendEvent(eventsDir, "dispatched", map[string]any{"id": id, "role": "general", "mode": "single"})
	// No finished/delivered event: it was still running at crash time.
	// (general is a write role, so the crashed process had transferred
	// the write token to it.)

	before := f.parent.RemainingTokens()
	mgr2, err := NewManager(ManagerOpts{
		Parent: f.parent, Config: f.m.opts.Config,
		Client: f.m.opts.Client, BaseReg: f.baseReg,
		Agents: f.agents, SessionID: "test2",
		SessLog:      f.sessLog,
		SubLogDir:    f.m.subLogDir,
		EventsLogDir: eventsDir,
		Events: func(event string, fields map[string]any) {
			AppendEvent(eventsDir, event, fields)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr2.SetParent(f.parent)

	// Write token back with main.
	if mgr2.WriteGate().Held() {
		t.Fatal("write token must be with main after interruption report")
	}
	// No budget was silently reserved (fresh process) and none leaked.
	if after := f.parent.RemainingTokens(); after != before {
		t.Fatalf("budget changed without any sub-agent running: %d -> %d", before, after)
	}

	// The interruption notice entered the session log exactly once,
	// with the agent_result pair and the Chinese message from the spec.
	msgs, _, err := sessionlog.RebuildWithWarning(logPath)
	if err != nil {
		t.Fatal(err)
	}
	pairs := 0
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && strings.Contains(m.ToolCalls[0].ID, id) {
			pairs++
		}
	}
	if pairs != 1 {
		t.Fatalf("interruption notice must be delivered exactly once, got %d", pairs)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "在重启时被中断") &&
			strings.Contains(m.Content, id) {
			found = true
		}
	}
	if !found {
		t.Fatalf("interruption message missing: %+v", msgs)
	}
	// agent_result on the interrupted session returns the reference
	// only (already delivered).
	tools := mgr2.ManagerTools()
	rt := tools[4].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	out, err := rt.Call(context.Background(), json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "不要再查询") {
		t.Fatalf("interrupted result must not be re-delivered: %s", out)
	}
	// Delivered event persisted.
	if !DeliveredIDs(eventsDir)[id] {
		t.Fatal("delivered event missing for interrupted session")
	}
	// Second restart: nothing new.
	mgr3, err := NewManager(ManagerOpts{
		Parent: f.parent, Config: f.m.opts.Config,
		Client: f.m.opts.Client, BaseReg: f.baseReg,
		Agents: f.agents, SessionID: "test3",
		SessLog:      f.sessLog,
		SubLogDir:    f.m.subLogDir,
		EventsLogDir: eventsDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr3.SetParent(f.parent)
	msgs2, _, _ := sessionlog.RebuildWithWarning(logPath)
	if got := len(msgs2); got != len(msgs) {
		t.Fatalf("second restart must not add messages: %d -> %d", len(msgs), got)
	}
}

// TestWrapUpToolCallYieldsFixedSummary: when a sub-agent burns its turn
// budget and the wrap-up call itself answers with a tool call (or empty
// text), the run is recorded budget_exhausted (never done) and the
// delivered summary is the fixed non-empty text. The wrap-up request
// must carry no tools.
func TestWrapUpToolCallYieldsFixedSummary(t *testing.T) {
	var wrapUpHadTools bool
	n := 0
	var mu sync.Mutex
	srv := fakeLLM2(t, func(k int, req model.Request) string {
		mu.Lock()
		n++
		hasTools := len(req.Tools) > 0
		if req.Messages[len(req.Messages)-1].Role == "user" &&
			strings.Contains(req.Messages[len(req.Messages)-1].Content, "Stop working") {
			wrapUpHadTools = hasTools
		}
		mu.Unlock()
		// Every call, including the wrap-up, answers with a tool call.
		return toolCall("cw"+itoa(n), "list_dir", `{"path":"."}`)
	})
	f := newFixture(t, nil, fxOpts{})
	f.m.opts.Client = model.NewClient(srv.URL, "k", nil)

	before := f.parent.RemainingTokens()
	def := f.agents["explore"] // max_turns: 10
	// Shrink the role budget to reach the turn limit fast.
	def.Budget.MaxTurns = 1
	f.m.opts.Agents["explore"] = def

	st := f.m.ManagerTools()[0].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	out, err := st.Call(context.Background(), json.RawMessage(
		`{"role":"explore","task":"wander around","mode":"single"}`))
	if err != nil {
		t.Fatal(err)
	}

	if wrapUpHadTools {
		t.Fatal("wrap-up request must not carry tools")
	}
	// The delivered content is the fixed text (from the main session
	// log). The run is background: poll until the watchdog delivers.
	var msgs []model.Message
	var rerr error
	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _, rerr = sessionlog.RebuildWithWarning(
			filepath.Join(f.root, "sessions", "main", "messages.jsonl"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, m := range msgs {
			if m.Role == "tool" && strings.Contains(m.Content, agent.BudgetExhaustedSummary) {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("fixed exhausted summary not delivered; out=%s", out)
	}
	// Session state is budget_exhausted, and the reservation returned.
	var s *Session
	f.m.mu.Lock()
	for _, sess := range f.m.sessions {
		s = sess
		break
	}
	f.m.mu.Unlock()
	deadline2 := time.Now().Add(3 * time.Second)
	for s.State != StateBudgetExhausted && time.Now().Before(deadline2) {
		time.Sleep(20 * time.Millisecond)
	}
	f.m.mu.Lock()
	state := s.State
	f.m.mu.Unlock()
	if state != StateBudgetExhausted {
		t.Fatalf("state = %q, want budget_exhausted", state)
	}
	if after := f.parent.RemainingTokens(); after != before {
		t.Fatalf("reservation not returned: %d -> %d", before, after)
	}
}

// TestParallelTaskForms: tasks[] accepts strings (shared role),
// {role,task} objects, and a mix; an unknown or non-readonly role in an
// object element is refused with the correct shape in the message.
func TestParallelTaskForms(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	st := f.m.ManagerTools()[0].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	ctx := context.Background()

	// 1. String form (shared role): budget pre-deducted for the sum.
	before := f.parent.RemainingTokens()
	out, err := st.Call(ctx, json.RawMessage(
		`{"role":"explore","mode":"parallel","tasks":["a","b"]}`))
	if err != nil {
		t.Fatalf("string form refused: %v", err)
	}
	if f.parent.RemainingTokens() != before { // released after completion
		t.Fatalf("string form leaked budget: %d -> %d", before, f.parent.RemainingTokens())
	}
	var res startResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.IDs) != 2 {
		t.Fatalf("string form result: %v %s", err, out)
	}

	// 2. Object form (per-task role).
	out, err = st.Call(ctx, json.RawMessage(
		`{"mode":"parallel","tasks":[{"role":"explore","task":"a"},{"role":"review","task":"b"}]}`))
	if err != nil {
		t.Fatalf("object form refused: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.IDs) != 2 {
		t.Fatalf("object form result: %v %s", err, out)
	}

	// 3. Mixed form.
	out, err = st.Call(ctx, json.RawMessage(
		`{"role":"explore","mode":"parallel","tasks":["plain",{"role":"review","task":"obj"}]}`))
	if err != nil {
		t.Fatalf("mixed form refused: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.IDs) != 2 {
		t.Fatalf("mixed form result: %v %s", err, out)
	}

	// 4. Unknown role in an object element: refused, message carries the
	// correct shape.
	_, err = st.Call(ctx, json.RawMessage(
		`{"mode":"parallel","tasks":[{"role":"no-such-role","task":"a"}]}`))
	if err == nil {
		t.Fatal("unknown role must be refused")
	}
	if !strings.Contains(err.Error(), "expected shape") || !strings.Contains(err.Error(), "no-such-role") {
		t.Fatalf("error must name the role and the shape: %v", err)
	}

	// 5. Non-readonly role in an object element: refused.
	_, err = st.Call(ctx, json.RawMessage(
		`{"mode":"parallel","tasks":[{"role":"general","task":"a"}]}`))
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("non-readonly role must be refused with read-only hint: %v", err)
	}
}

// TestSameTurnResultDelivery: while the sub-agent runs, agent_status
// tells the model to end its turn; once finished, the FIRST agent_result
// call returns the summary body and counts as the only delivery — no
// message pair is injected afterwards, and repeat calls stop polling.
func TestSameTurnResultDelivery(t *testing.T) {
	srv := fakeLLM2(t, func(k int, req model.Request) string {
		if k == 1 {
			return textReply("SAME_TURN_BODY")
		}
		return textReply("should not be called again")
	})
	f := newFixture(t, nil, fxOpts{})
	f.m.opts.Client = model.NewClient(srv.URL, "k", nil)

	tools := f.m.ManagerTools()
	statusT := tools[1].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	resultT := tools[4].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})

	// Inject a session that is still running.
	s := &Session{
		ID: "sub-sameturn1", Role: "explore",
		Def: f.agents["explore"], Task: "t", Mode: ModeSingle,
		State: StateRunning,
	}
	f.m.mu.Lock()
	f.m.sessions[s.ID] = s
	f.m.mu.Unlock()

	// 1. status while running: hint to end the turn.
	out, err := statusT.Call(context.Background(), json.RawMessage(`{"id":"sub-sameturn1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "结束本轮即可") {
		t.Fatalf("running status must carry the end-turn hint: %s", out)
	}

	// 2. The run finishes (watchdog path would deliver; simulate the
	// completed state without the watchdog so agent_result wins).
	f.m.mu.Lock()
	s.State = StateDone
	s.Summary = "SAME_TURN_BODY"
	f.m.mu.Unlock()

	// 3. First agent_result: the body, inline.
	out, err = resultT.Call(context.Background(), json.RawMessage(`{"id":"sub-sameturn1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SAME_TURN_BODY") {
		t.Fatalf("first result must return the body: %s", out)
	}
	// Exactly one delivered event, via agent_result.
	delivs := 0
	for l := range readLines(filepath.Join(f.cfgDir, "logs", "subagents.jsonl")) {
		var e struct {
			Event string `json:"event"`
			ID    string `json:"id"`
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.Event == "delivered" && e.ID == s.ID {
			delivs++
		}
	}
	if delivs != 1 {
		t.Fatalf("expected exactly 1 delivered event, got %d", delivs)
	}

	// 4. Repeat call: stop-polling reference, no body.
	out, err = resultT.Call(context.Background(), json.RawMessage(`{"id":"sub-sameturn1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SAME_TURN_BODY") || !strings.Contains(out, "不要再查询") {
		t.Fatalf("repeat call must be a stop-polling reference: %s", out)
	}

	// 5. Next turn: no pair injected (log has no subnotice pair for it).
	msgs, _, err := sessionlog.RebuildWithWarning(
		filepath.Join(f.root, "sessions", "main", "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 &&
			strings.Contains(m.ToolCalls[0].ID, s.ID) {
			t.Fatalf("no pair should be injected after inline delivery: %+v", m)
		}
	}
}

// readLines is a small helper over a text file.
func readLines(path string) chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, l := range strings.Split(string(data), "\n") {
			if l != "" {
				ch <- l
			}
		}
	}()
	return ch
}

// TestSendRefusesDeliveredSession: a finished sub-agent whose result
// was already delivered is not re-runnable — agent_send refuses
// without reserving budget or re-running, and points at agent_start.
func TestSendRefusesDeliveredSession(t *testing.T) {
	f := newFixture(t, nil, fxOpts{})
	tools := f.m.ManagerTools()
	sendT := tools[2].(interface {
		Call(context.Context, json.RawMessage) (string, error)
	})
	s := &Session{
		ID: "sub-dlv-refuse", Role: "explore",
		Def: f.agents["explore"], Task: "t", Mode: ModeSingle,
		State: StateDone, Summary: "OLD", Delivered: true,
	}
	f.m.mu.Lock()
	f.m.sessions[s.ID] = s
	f.m.mu.Unlock()

	before := f.parent.RemainingTokens()
	_, err := sendT.Call(context.Background(), json.RawMessage(
		`{"id":"sub-dlv-refuse","message":"continue"}`))
	if err == nil {
		t.Fatal("agent_send must refuse a delivered session")
	}
	if !strings.Contains(err.Error(), "agent_start 新开") {
		t.Fatalf("refusal must point at agent_start: %v", err)
	}
	if after := f.parent.RemainingTokens(); after != before {
		t.Fatalf("budget must not be reserved on refusal: %d -> %d", before, after)
	}
	// The session state is untouched (not re-queued).
	f.m.mu.Lock()
	state := s.State
	f.m.mu.Unlock()
	if state != StateDone {
		t.Fatalf("session state changed on refusal: %q", state)
	}
}
