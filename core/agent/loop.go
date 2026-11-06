package agent

// Goal restatement and loop detection. A todo tool maintains
// <session dir>/todo.md; each model call gets a compact restatement
// (<= maxRestateBytes) appended after the conversation — never inside
// the stable prefix. A rolling signature of recent tool calls
// (sha256 of tool name + normalized input + output) detects loops:
// after N repeats a reminder is injected; one more repeat after the
// reminder terminates the run like a budget stop.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

type contextType = context.Context

// LoopConfig tunes detection; zero fields use defaults.
type LoopConfig struct {
	// Window is the rolling window size (default 10).
	Window int
	// Threshold is repeats of one signature within the window before
	// a reminder is injected (default 5).
	Threshold int
	// MaxRestateBytes caps the todo restatement (default 500).
	MaxRestateBytes int
}

func (c LoopConfig) window() int {
	if c.Window > 0 {
		return c.Window
	}
	return 10
}
func (c LoopConfig) threshold() int {
	if c.Threshold > 0 {
		return c.Threshold
	}
	return 5
}
func (c LoopConfig) restateBytes() int {
	if c.MaxRestateBytes > 0 {
		return c.MaxRestateBytes
	}
	return 500
}

// TodoTool maintains todo.md in the session directory.
type TodoTool struct{ Dir string }

func (t TodoTool) todoPath() string { return filepath.Join(t.Dir, "todo.md") }

// Schema implements tool.Tool.
func (t TodoTool) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "todo",
		Description: "Maintain your working goals. Write the current objective and " +
			"unfinished steps to todo.md so the user can follow along and you can " +
			"recover context. Pass the complete file content each time.",
		Parameters: json.RawMessage(`{"type":"object",
 "properties":{"content":{"type":"string","description":"full markdown of goal + open steps"}},
 "required":["content"]}`),
	}}
}

// Call implements tool.Tool.
func (t TodoTool) Call(_ contextx, args json.RawMessage) (string, error) {
	var a struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Content) == "" {
		return "", fmt.Errorf("content must not be empty")
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(t.todoPath(), []byte(a.Content), 0o600); err != nil {
		return "", err
	}
	return "todo saved", nil
}

type contextx = contextType

// restate reads todo.md and trims to the byte budget.
func (t TodoTool) restate() string {
	data, err := os.ReadFile(t.todoPath())
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}
	const budget = 500
	if len(text) > budget {
		lines := strings.Split(text, "\n")
		var kept []string
		used := 0
		for _, l := range lines {
			if used+len(l)+1 > budget {
				break
			}
			kept = append(kept, l)
			used += len(l) + 1
		}
		text = strings.Join(kept, "\n") + "\n... (todo trimmed)"
	}
	return "[CURRENT GOAL]\n" + text
}

// loopDetector tracks recent tool-call signatures.
type loopDetector struct {
	cfg       LoopConfig
	window    []string
	reminded  bool // reminder already injected for the current repeat run
	lastSig   string
	lastCount int
}

// signature hashes tool name + normalized input + output. Whitespace
// is normalized so retries with cosmetically different payloads that
// produce the same result still count.
func loopSignature(name, input, output string) string {
	norm := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	sum := sha256.Sum256([]byte(name + "\x00" + norm(input) + "\x00" + norm(output)))
	return hex.EncodeToString(sum[:])
}

// observe records one tool call and returns an action:
// "" (ok) | "remind" | "stop".
func (d *loopDetector) observe(name, input, output string) string {
	sig := loopSignature(name, input, output)
	d.window = append(d.window, sig)
	if len(d.window) > d.cfg.window() {
		d.window = d.window[len(d.window)-d.cfg.window():]
	}
	count := 0
	for _, s := range d.window {
		if s == sig {
			count++
		}
	}
	if count > d.cfg.threshold() {
		// One more occurrence after the reminder: stop.
		if d.reminded && sig == d.lastSig {
			return "stop"
		}
	}
	if count >= d.cfg.threshold() && !d.reminded {
		d.reminded = true
		d.lastSig = sig
		return "remind"
	}
	if count < d.cfg.threshold() {
		d.reminded = false
		d.lastSig = ""
	}
	return ""
}

var loopReminder = "System: you are repeating the same tool call with the same result. " +
	"Change approach, or summarize and finish."

// injectRestate appends the todo restatement to the messages sent to
// the model (a view, never persisted into history).
func (a *Agent) viewForModel() []model.Message {
	if a.todo == nil {
		return a.messages
	}
	r := a.todo.restate()
	if r == "" {
		return a.messages
	}
	out := make([]model.Message, 0, len(a.messages)+1)
	out = append(out, a.messages...)
	out = append(out, model.Message{Role: model.RoleUser, Content: r})
	return out
}

var _ = sort.Strings
