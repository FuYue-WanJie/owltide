package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// Fake model that repeats the identical tool call until told to stop;
// after receiving the loop reminder it repeats once more (and must be
// terminated), unless the script answers normally.
func loopScript(t *testing.T, stopAfterReminder bool, uniquePerCall bool) func(n int, req model.Request) string {
	return func(n int, req model.Request) string {
		// Wrap-up / system prompts: answer with text.
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.Contains(last.Content, loopReminder) {
			if !stopAfterReminder {
				// well-behaved: change approach and finish
				b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
					"delta": map[string]any{"content": "changed approach"},
				}}})
				return chunk(string(b)) + sseDone()
			}
			// repeat offender: fall through to another identical tool call
			cmd := "echo loop"
			a, _ := json.Marshal(map[string]string{"command": cmd})
			tc := map[string]any{
				"id": "call-r" + itoa(n), "type": "function",
				"function": map[string]string{"name": "run_shell", "arguments": string(a)},
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{tc}},
			}}})
			return chunk(string(b)) + sseDone()
		}
		if last.Role == "user" && strings.Contains(last.Content, "System:") {
			// wrap-up prompt: comply
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "stopped"},
			}}})
			return chunk(string(b)) + sseDone()
		}
		cmd := "echo loop"
		if uniquePerCall {
			cmd = "echo loop-" + itoa(n)
		}
		a, _ := json.Marshal(map[string]string{"command": cmd})
		tc := map[string]any{
			"id": "call-l" + itoa(n), "type": "function",
			"function": map[string]string{"name": "run_shell", "arguments": string(a)},
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}}})
		return chunk(string(b)) + sseDone()
	}
}

func jsonArgs(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

func itoa(n int) string {
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

func loopAgent(t *testing.T, srv string, dl *decision.Logger) (*Agent, string) {
	t.Helper()
	todoDir := t.TempDir()
	cfg := config.Default()
	cfg.Budget.MaxTurns = 30
	reg := tool.NewRegistry()
	must(reg.Register(tool.NewShell("/bin/sh", "-c", t.TempDir(), 5)))
	opts := agentOptions(cfg, model.NewClient(srv, "k", nil), reg, dl)
	opts.TodoDir = todoDir
	return New(opts), todoDir
}

// Reminder then stop on identical repeats; decision log records it.
func TestLoopDetectedReminderThenStop(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dtmp := t.TempDir()
	dl, err := decision.Open(dtmp)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()
	srv := fakeLLM(t, 99, loopScript(t, true, false))
	a, _ := loopAgent(t, srv.URL, dl)

	_, err = a.Run(context.Background(), "loop please")
	if err == nil || !strings.Contains(err.Error(), "loop detected") {
		t.Fatalf("want loop stop, got %v", err)
	}

	// Reminder was injected: the transcript contains it.
	foundReminder := false
	for _, m := range a.messages {
		if strings.Contains(m.Content, loopReminder) {
			foundReminder = true
		}
	}
	if !foundReminder {
		t.Fatal("loop reminder not injected before termination")
	}

	// Decision log records the loop stop.
	entries, _ := os.ReadDir(dtmp)
	found := false
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dtmp, e.Name()))
		if strings.Contains(string(b), "loop stop: repeated identical tool call") {
			found = true
		}
	}
	if !found {
		t.Fatal("loop stop not recorded in decision log")
	}
}

// Normal retries with different outputs must not trip the detector.
func TestLoopNoFalsePositiveOnUniqueOutputs(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dtmp := t.TempDir()
	dl, err := decision.Open(dtmp)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()
	srv := fakeLLM(t, 99, func(n int, req model.Request) string {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.Contains(last.Content, "System:") {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "done"},
			}}})
			return chunk(string(b)) + sseDone()
		}
		if n >= 8 { // finish before turn limit
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "finished"},
			}}})
			return chunk(string(b)) + sseDone()
		}
		a, _ := json.Marshal(map[string]string{"command": "echo step-" + itoa(n)})
		tc := map[string]any{
			"id": "call-u" + itoa(n), "type": "function",
			"function": map[string]string{"name": "run_shell", "arguments": string(a)},
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}}})
		return chunk(string(b)) + sseDone()
	})
	a, _ := loopAgent(t, srv.URL, dl)
	_, err = a.Run(context.Background(), "do unique steps")
	if err != nil {
		t.Fatalf("unique retries must not trip the detector: %v", err)
	}
}

// The todo tool writes todo.md and the restatement is appended after
// the conversation (not inside the stable prefix).
func TestTodoRestateAppended(t *testing.T) {
	srv := fakeLLM(t, 2, func(n int, req model.Request) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "ok"},
		}}})
		return chunk(string(b)) + sseDone()
	})
	a, todoDir := loopAgent(t, srv.URL, nil)

	td := TodoTool{Dir: todoDir}
	if _, err := td.Call(context.Background(), json.RawMessage(`{"content":"# Goal\n- ship it"}`)); err != nil {
		t.Fatal(err)
	}
	// File exists.
	if _, err := os.Stat(filepath.Join(todoDir, "todo.md")); err != nil {
		t.Fatal(err)
	}
	// Restate appends after history, does not mutate a.messages.
	before := len(a.messages)
	view := a.viewForModel()
	if len(view) != before+1 {
		t.Fatalf("restate must append exactly one message: %d -> %d", before, len(view))
	}
	if !strings.Contains(view[len(view)-1].Content, "ship it") {
		t.Fatalf("restate missing goal: %q", view[len(view)-1].Content)
	}
	// The stable prefix (messages[0]) untouched.
	if a.messages[0].Content != view[0].Content {
		t.Fatal("restate must not touch the prefix")
	}
	// Over 500 bytes gets trimmed.
	huge := strings.Repeat("x", 900)
	td.Call(context.Background(), json.RawMessage(`{"content":"`+huge+`"}`))
	view2 := a.viewForModel()
	if len(view2[len(view2)-1].Content) > 600 {
		t.Fatalf("restate not trimmed: %d bytes", len(view2[len(view2)-1].Content))
	}
}
