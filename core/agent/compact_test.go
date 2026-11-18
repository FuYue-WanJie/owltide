package agent

// P2b-M6/M7 agent-level tests: turn-boundary trigger, prefix hash
// change exactly once per fold, decision events, and the reversible
// context tools.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/compaction"
	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

func compactAgent(t *testing.T, ctxTokens int, ratio float64) (*Agent, *[]compaction.Record) {
	t.Helper()
	reg := tool.NewRegistry()
	cfg := config.Default()
	cfg.Model.BaseURL = "https://x"
	opts := agentOptions(cfg, model.NewClient("https://x", "k", nil), reg, nil)
	var recs []compaction.Record
	opts.Compact = &CompactOptions{
		ContextTokens: ctxTokens, Ratio: ratio,
		Persist: func(r compaction.Record) error { recs = append(recs, r); return nil },
	}
	return New(opts), &recs
}

// TestCompactAtTurnBoundary: a long turn does NOT compact mid-turn;
// the fold happens only when the turn ends.
func TestCompactAtTurnBoundary(t *testing.T) {
	a, recs := compactAgent(t, 200, 0.7)
	// Simulate a mid-turn history by appending messages directly.
	for i := 0; i < 3; i++ {
		a.logMsg(model.Message{Role: model.RoleUser, Content: "u" + string(rune('a'+i)) + strings.Repeat("x", 300)})
		a.logMsg(model.Message{Role: model.RoleAssistant, Content: strings.Repeat("y", 300)})
	}
	if len(*recs) != 0 {
		t.Fatal("compaction must not run mid-turn")
	}
	changed := a.maybeCompact()
	if !changed || len(*recs) == 0 {
		t.Fatal("boundary compaction did not fire")
	}
}

// TestPrefixHashChangesOnlyOnFold: the prefix hash is stable across
// no-op turns and changes exactly when a fold happens.
func TestPrefixHashChangesOnlyOnFold(t *testing.T) {
	a, _ := compactAgent(t, 100000, 0.7) // never over threshold
	h0 := a.prefixHash
	a.logMsg(model.Message{Role: model.RoleUser, Content: "hi"})
	if a.maybeCompact() {
		t.Fatal("no fold expected under threshold")
	}
	if a.prefixHash != h0 {
		t.Fatal("prefix changed without a fold")
	}

	// Force a fold with a tiny window.
	a.opts.Compact.ContextTokens = 200
	for i := 0; i < 4; i++ {
		a.logMsg(model.Message{Role: model.RoleUser, Content: "u " + strings.Repeat("z", 300)})
		a.logMsg(model.Message{Role: model.RoleAssistant, Content: strings.Repeat("w", 300)})
	}
	if !a.maybeCompact() {
		t.Fatal("fold expected")
	}
	h1 := a.prefixHash
	if h1 == h0 {
		t.Fatal("prefix hash must change on a fold")
	}
	// A follow-up no-op keeps the new hash.
	if a.maybeCompact() {
		t.Fatal("unexpected second fold")
	}
	if a.prefixHash != h1 {
		t.Fatal("prefix changed on a no-op boundary")
	}
}

// TestContextExpandSearch: originals are retrievable by span and
// searchable after folding.
func TestContextExpandSearch(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "messages.jsonl")
	var lines []string
	full := []model.Message{
		{Role: model.RoleUser, Content: "第一次: 请修复 calc 的 bug"},
		{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{ID: "t1", Type: "function",
			Function: model.Function{Name: "run_shell", Arguments: `{"command":"go test"}`}}}},
		{Role: model.RoleTool, Name: "run_shell", ToolCallID: "t1", Content: strings.Repeat("OUTPUT ", 2000)},
		{Role: model.RoleAssistant, Content: "修复完成"},
		{Role: model.RoleUser, Content: "第二个任务: 加测试"},
	}
	for _, m := range full {
		b, _ := json.Marshal(m)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ct := ContextTools{LogPath: logPath}
	out, err := ct.Call(context.Background(), json.RawMessage(`{"from":0,"to":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "修复 calc") {
		t.Fatalf("expand missing content:\n%s", out[:300])
	}
	// Page through to find the later assistant text.
	page, offset, found := out, 8000, strings.Contains(out, "修复完成")
	for i := 0; i < 5 && !found; i++ {
		page, err = ct.Call(context.Background(),
			json.RawMessage(fmt.Sprintf(`{"from":0,"to":3,"next_offset":%d}`, offset)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(page, "修复完成") {
			found = true
		}
		if j := strings.Index(page, "next_offset="); j >= 0 {
			fmt.Sscanf(page[j+len("next_offset="):], "%d", &offset)
		} else {
			break
		}
	}
	if !found {
		t.Fatal("later content never found across pages")
	}
	// Pagination: a big span splits.
	out2, err := ct.Call(context.Background(), json.RawMessage(`{"from":2,"to":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "next_offset=") {
		t.Fatalf("big span must paginate:\n%s", out2[:200])
	}
	out3, err := ct.Call(context.Background(), json.RawMessage(`{"from":99,"to":100}`))
	if err == nil {
		t.Fatal("out-of-range span must error")
	}
	_ = out3

	cs := ContextSearch{LogPath: logPath}
	res, err := cs.Call(context.Background(), json.RawMessage(`{"query":"第二个任务"}`))
	if err != nil || !strings.Contains(res, "#4") {
		t.Fatalf("search miss: %v %q", err, res)
	}
	res, err = cs.Call(context.Background(), json.RawMessage(`{"query":"OUTPUT OUTPUT"}`))
	if err != nil || !strings.Contains(res, "#2") {
		t.Fatalf("search in tool output miss: %v %q", err, res)
	}
	res, _ = cs.Call(context.Background(), json.RawMessage(`{"query":"不存在的词"}`))
	if !strings.Contains(res, "no match") {
		t.Fatalf("empty search = %q", res)
	}
}
