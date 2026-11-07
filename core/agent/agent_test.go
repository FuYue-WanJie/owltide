package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/guard"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/sessionlog"
	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// fakeLLM drives a scripted multi-turn conversation:
// turn 0 asks to run a tool; turn 1 reads the tool result and answers.
func fakeLLM(t *testing.T, calls int, script func(n int, req model.Request) string) *httptest.Server {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req model.Request
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(400)
			return
		}
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(script(n, req)))
		if n >= calls {
			// keep answering summary for extra calls
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chunk(c string) string {
	return "data: " + c + "\n\n"
}

func sseDone() string { return "data: [DONE]\n\n" }

func TestAgentLoopWithToolAndDecisionLog(t *testing.T) {
	tmp := t.TempDir()
	workFile := filepath.Join(tmp, "note.txt")

	srv := fakeLLM(t, 2, func(n int, req model.Request) string {
		switch n {
		case 1:
			// Model requests write_file.
			args, _ := json.Marshal(map[string]string{"path": workFile, "content": "owltide was here"})
			tc := map[string]any{
				"id": "call-1", "type": "function",
				"function": map[string]string{"name": "write_file", "arguments": string(args)},
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{tc}},
			}}})
			return chunk(string(b)) + sseDone()
		default:
			// Model sees the tool result and answers; check the history contains it.
			found := false
			for _, m := range req.Messages {
				if m.Role == "tool" && strings.Contains(m.Content, "wrote") {
					found = true
				}
			}
			if !found {
				t.Error("tool result missing from model history")
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "done"},
			}}})
			return chunk(string(b)) + sseDone()
		}
	})

	cfg := config.Default()
	cfg.Model.Name = "fake-model"
	cfg.Budget.MaxTurns = 10

	reg := tool.NewRegistry()
	must(reg.Register(&tool.WriteFile{}))

	dl, err := decision.Open(filepath.Join(tmp, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	opts := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, dl)
	a := New(opts)
	reply, err := a.Run(context.Background(), "write a note")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" {
		t.Fatalf("reply: %q", reply)
	}
	data, err := os.ReadFile(workFile)
	if err != nil || string(data) != "owltide was here" {
		t.Fatalf("file: %q %v", data, err)
	}

	// Decision log must have exactly one line with expected fields.
	logData, err := os.ReadFile(filepath.Join(tmp, "logs", "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 decision line, got %d", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ts", "session_id", "turn", "input_digest", "model",
		"prompt_tokens", "completion_tokens", "tools_called", "replied_directly",
		"duration_ms", "outcome"} {
		if _, ok := rec[field]; !ok {
			t.Errorf("decision record missing field %q", field)
		}
	}
	if rec["outcome"] != "success" {
		t.Errorf("outcome = %v", rec["outcome"])
	}
	if rec["replied_directly"] != false {
		t.Errorf("replied_directly should be false when tools were used")
	}
}

func TestAgentTurnLimit(t *testing.T) {
	// Model always requests a tool; agent must stop at max turns.
	srv := fakeLLM(t, 99, func(n int, req model.Request) string {
		// Wrap-up prompt: reply with a summary instead of another tool call.
		if len(req.Messages) > 0 {
			last := req.Messages[len(req.Messages)-1]
			if last.Role == "user" && strings.Contains(last.Content, "System:") {
				b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
					"delta": map[string]any{"content": "stopped at turn limit"},
				}}})
				return chunk(string(b)) + sseDone()
			}
		}
		args, _ := json.Marshal(map[string]string{"command": "echo loop"})
		tc := map[string]any{
			"id": "call-x", "type": "function",
			"function": map[string]string{"name": "run_shell", "arguments": string(args)},
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}}})
		return chunk(string(b)) + sseDone()
	})

	cfg := config.Default()
	cfg.Budget.MaxTurns = 3

	reg := tool.NewRegistry()
	sh := tool.NewShell("/bin/sh", "-c", t.TempDir(), 5)
	must(reg.Register(sh))

	opts := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, nil)
	a := New(opts)
	reply, err := a.Run(context.Background(), "loop forever")
	if err != nil {
		t.Fatal(err)
	}
	if reply == "" {
		t.Fatal("expected a wrap-up summary")
	}
}

// TestAgentBudgetStopLogged verifies that a budget-forced wrap-up is
// recorded in the decision log as an error outcome.
func TestAgentBudgetStopLogged(t *testing.T) {
	srv := fakeLLM(t, 99, func(n int, req model.Request) string {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.Contains(last.Content, "System:") {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "stopped"},
			}}})
			return chunk(string(b)) + sseDone()
		}
		args, _ := json.Marshal(map[string]string{"command": "echo loop"})
		tc := map[string]any{
			"id": "call-x", "type": "function",
			"function": map[string]string{"name": "run_shell", "arguments": string(args)},
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}}})
		return chunk(string(b)) + sseDone()
	})

	tmp := t.TempDir()
	dl, err := decision.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	cfg := config.Default()
	cfg.Budget.MaxTurns = 3

	reg := tool.NewRegistry()
	sh := tool.NewShell("/bin/sh", "-c", t.TempDir(), 5)
	must(reg.Register(sh))

	opts := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, dl)
	a := New(opts)
	reply, err := a.Run(context.Background(), "loop forever")
	if err != nil {
		t.Fatal(err)
	}
	if reply == "" {
		t.Fatal("expected a wrap-up summary")
	}

	// Read the decision log and check the budget stop is recorded.
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(tmp, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, `"outcome":"error"`) &&
				strings.Contains(line, "budget stop: turn_limit") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("budget stop not recorded in decisions log")
	}
}

// TestDangerousCommandLogged verifies a confirmation-gated rejection
// lands in the decision log via the tool-error path.
func TestDangerousCommandLogged(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	srv := fakeLLM(t, 2, func(n int, req model.Request) string {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.Contains(last.Content, "System:") ||
			strings.Contains(last.Content, "(empty output)") == false && last.Role == "user" && n > 1 {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "understood"},
			}}})
			return chunk(string(b)) + sseDone()
		}
		args, _ := json.Marshal(map[string]string{"command": "rm -rf /tmp/nothing-here"})
		tc := map[string]any{
			"id": "call-d", "type": "function",
			"function": map[string]string{"name": "run_shell", "arguments": string(args)},
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}}})
		return chunk(string(b)) + sseDone()
	})

	tmp := t.TempDir()
	dl, err := decision.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	cfg := config.Default()
	reg := tool.NewRegistry()
	// No confirmer: rejection is the default in non-interactive mode.
	must(reg.Register(tool.NewShell("/bin/sh", "-c", t.TempDir(), 5)))

	opts := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, dl)
	a := New(opts)
	if _, err := a.Run(context.Background(), "delete the temp dir"); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(tmp)
	var found bool
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(tmp, e.Name()))
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "user confirmation") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("confirmation rejection not recorded in decision log")
	}
}

// TestGuardRollsBackShellTampering: run_shell editing locks.yaml is
// rolled back at the transaction layer and the violation is returned
// to the model and logged.
func TestGuardRollsBackShellTampering(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".owltide")
	for _, d := range []string{"", "logs", "agents"} {
		os.MkdirAll(filepath.Join(cfgDir, d), 0o755)
	}
	os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("model:\n  name: m\n  base_url: https://x\n"), 0o600)
	os.WriteFile(filepath.Join(cfgDir, "locks.yaml"), []byte("locked:\n    - agent:reviewer\n"), 0o600)
	st, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureInit(); err != nil {
		t.Fatal(err)
	}
	gd := &guard.Guard{Root: cfgDir, St: st}

	srv := fakeLLM(t, 2, func(n int, req model.Request) string {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && n > 1 {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "got it"},
			}}})
			return chunk(string(b)) + sseDone()
		}
		cmd := fmt.Sprintf("echo 'locked: []' > %s/locks.yaml", cfgDir)
		args, _ := json.Marshal(map[string]string{"command": cmd})
		tc := map[string]any{
			"id": "call-t", "type": "function",
			"function": map[string]string{"name": "run_shell", "arguments": string(args)},
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}}})
		return chunk(string(b)) + sseDone()
	})

	dtmp := t.TempDir()
	dl, err := decision.Open(dtmp)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	cfg := config.Default()
	reg := tool.NewRegistry()
	sh := tool.NewShell("/bin/sh", "-c", root, 5)
	// Simulate a user approving the dangerous command: the write goes
	// through and the transaction guard must still roll it back.
	sh.SetConfirmer(func(string) bool { return true })
	must(reg.Register(sh))

	opts := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, dl)
	opts.Guard = gd
	a := New(opts)
	if _, err := a.Run(context.Background(), "clear the locks"); err != nil {
		t.Fatal(err)
	}

	// locks.yaml must be restored.
	data, err := os.ReadFile(filepath.Join(cfgDir, "locks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "agent:reviewer") {
		t.Fatalf("locks.yaml not restored: %q", data)
	}

	// Violation recorded in the decision log.
	entries, _ := os.ReadDir(dtmp)
	var found bool
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dtmp, e.Name()))
		if strings.Contains(string(b), "PROTECTED FILE VIOLATION") {
			found = true
		}
	}
	if !found {
		t.Fatal("violation not recorded in decision log")
	}
}

// TestStablePrefixBytes: two consecutive turns of the same session
// must serialize the request prefix to identical bytes.
func TestStablePrefixBytes(t *testing.T) {
	srv := fakeLLM(t, 2, func(n int, req model.Request) string {
		// Turn 1: use a tool; turn 2: answer.
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"path": "/tmp/prefix-probe.txt"})
			tc := map[string]any{
				"id": "call-p", "type": "function",
				"function": map[string]string{"name": "read_file", "arguments": string(args)},
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{tc}},
			}}})
			return chunk(string(b)) + sseDone()
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "done"},
		}}})
		return chunk(string(b)) + sseDone()
	})
	os.WriteFile("/tmp/prefix-probe.txt", []byte("data"), 0o644)

	cfg := config.Default()
	reg := tool.NewRegistry()
	must(reg.Register(tool.ReadFile{}))
	must(reg.Register(&tool.WriteFile{}))

	// Capture every request body the fake server sees.
	var firstPrefix string
	srv2 := fakeLLM(t, 99, func(n int, req model.Request) string { return "" })
	_ = srv2

	opts := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, nil)
	a := New(opts)
	h1 := a.prefixHash
	if _, err := a.Run(context.Background(), "probe one"); err != nil {
		t.Fatal(err)
	}
	// Same agent instance, second turn: prefix must be identical.
	a2 := NewWithHistory(a, opts)
	h2 := a2.prefixHash
	_ = a2
	if h1 == "" {
		t.Fatal("empty prefix hash")
	}
	if h1 != h2 {
		t.Fatalf("prefix changed between turns: %s vs %s", h1, h2)
	}
	// And it must be reproducible from a fresh agent with the same
	// system prompt and tool registration order.
	opts2 := agentOptions(cfg, model.NewClient(srv.URL, "k", srv.Client()), reg, nil)
	a3 := New(opts2)
	if a3.prefixHash != h1 {
		t.Fatalf("prefix not reproducible: %s vs %s", a3.prefixHash, h1)
	}
	_ = firstPrefix
}

// textChunk is a plain assistant text reply in SSE form.
func textChunk(s string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": s},
	}}})
	return chunk(string(b)) + sseDone()
}

// TestResumeRebuildsConversation: a fresh agent restored from the
// session log continues the conversation — the model sees the earlier
// turns, and the prefix stays byte-identical.
func TestResumeRebuildsConversation(t *testing.T) {
	var mu sync.Mutex
	var reqs []model.Request
	srv := fakeLLM(t, 3, func(n int, req model.Request) string {
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		return textChunk("answer-" + itoaN(n))
	})

	cfg := config.Default()
	cfg.Model.Name = "m"
	cfg.Model.BaseURL = "https://x"
	reg := tool.NewRegistry()

	root := t.TempDir()
	log1, err := sessionlog.Open(root, "s1")
	must(err)
	opts1 := agentOptions(cfg, model.NewClient(srv.URL, "k", nil), reg, nil)
	opts1.SessLog = log1
	a1 := New(opts1)
	if _, err := a1.Run(context.Background(), "hello-one"); err != nil {
		t.Fatal(err)
	}
	must(log1.Close())

	msgs, warn, err := sessionlog.RebuildWithWarning(filepath.Join(root, "s1", "messages.jsonl"))
	if err != nil || warn != "" {
		t.Fatalf("rebuild: %v %q", err, warn)
	}
	if len(msgs) < 2 {
		t.Fatalf("log should hold user+assistant, got %d", len(msgs))
	}

	// "Restart": fresh agent, same log file appended to.
	log2, err := sessionlog.Open(root, "s1")
	must(err)
	defer log2.Close()
	opts2 := agentOptions(cfg, model.NewClient(srv.URL, "k", nil), reg, nil)
	opts2.SessLog = log2
	a2 := New(opts2)
	if a2.prefixHash != a1.prefixHash {
		t.Fatalf("prefix changed across restart: %s vs %s", a1.prefixHash, a2.prefixHash)
	}
	a2.RestoreHistory(msgs)
	if _, err := a2.Run(context.Background(), "hello-two"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(reqs))
	}
	// First call: system + user only.
	if len(reqs[0].Messages) != 2 {
		t.Fatalf("first call should carry 2 messages, got %d", len(reqs[0].Messages))
	}
	// Second call: the first turn must be present (system, user-one,
	// assistant-one, user-two).
	found1, foundA, found2 := false, false, false
	for _, m := range reqs[1].Messages {
		switch {
		case m.Role == model.RoleUser && strings.Contains(m.Content, "hello-one"):
			found1 = true
		case m.Role == model.RoleAssistant && strings.Contains(m.Content, "answer-1"):
			foundA = true
		case m.Role == model.RoleUser && strings.Contains(m.Content, "hello-two"):
			found2 = true
		}
	}
	if !found1 || !foundA || !found2 {
		t.Fatalf("resumed request missing history: user1=%v assistant1=%v user2=%v", found1, foundA, found2)
	}
	// The restored history is NOT re-persisted (no duplication).
	logPath := filepath.Join(root, "s1", "messages.jsonl")
	data, _ := os.ReadFile(logPath)
	if got := strings.Count(string(data), "hello-one"); got != 1 {
		t.Fatalf("restored history was re-persisted: hello-one appears %d times", got)
	}
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
