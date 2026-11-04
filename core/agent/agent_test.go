package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/model"
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
	must(reg.Register(tool.WriteFile{}))

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
