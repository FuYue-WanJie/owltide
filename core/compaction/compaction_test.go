package compaction

// P2b-M6 tests: threshold boundary, micro-before-turn ordering, tool
// pair integrity, sub-agent pair folding, incremental folds keeping
// old summaries byte-identical, spill ref preservation, and repeated
// compactions.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FuYue-WanJie/owltide/core/model"
)

func longTool(name, id string, size int) model.Message {
	return model.Message{Role: model.RoleTool, Name: name, ToolCallID: id, Content: strings.Repeat("x", size)}
}

// buildTurn assembles one completed turn: user, assistant(tool_call),
// tool result, final assistant text.
func buildTurn(n int, toolSize int) []model.Message {
	uid := "u" + itoa(n)
	return []model.Message{
		{Role: model.RoleUser, Content: "task " + itoa(n) + " do something useful"},
		{Role: model.RoleAssistant, Content: "", ToolCalls: []model.ToolCall{{
			ID: "tc" + uid, Type: "function",
			Function: model.Function{Name: "run_shell", Arguments: `{"command":"ls"}`},
		}}},
		longTool("run_shell", "tc"+uid, toolSize),
		{Role: model.RoleAssistant, Content: "done " + itoa(n) + " summary line\nmore"},
	}
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

func msgsOf(turns int, toolSize int) []model.Message {
	var out []model.Message
	for i := 0; i < turns; i++ {
		out = append(out, buildTurn(i, toolSize)...)
	}
	return out
}

func TestCompactAtResolution(t *testing.T) {
	if CompactAt(-1) != DefaultAt {
		t.Fatalf("unset ratio = %v, want %v", CompactAt(-1), DefaultAt)
	}
	if CompactAt(0) != 0 {
		t.Fatal("0 must disable")
	}
	if CompactAt(1.5) != 1 {
		t.Fatalf("clamped ratio = %v", CompactAt(1.5))
	}
	if Threshold(0, 0.7) != 0 || Threshold(1000, 0) != 0 {
		t.Fatal("zero window/ratio must mean off")
	}
	if got := Threshold(10000, 0.7); got != 7000 {
		t.Fatalf("threshold = %d, want 7000", got)
	}
}

func TestNoCompactionUnderThreshold(t *testing.T) {
	msgs := msgsOf(2, 100) // tiny
	res := Compact(msgs, Options{ContextTokens: 100000, Ratio: 0.7})
	if res.Changed {
		t.Fatalf("compacted under threshold (level %s)", res.Level)
	}
	if len(res.Messages) != len(msgs) {
		t.Fatal("messages changed")
	}
}

func TestDisableWithZeroRatio(t *testing.T) {
	msgs := msgsOf(6, 5000)
	res := Compact(msgs, Options{ContextTokens: 100, Ratio: 0})
	if res.Changed {
		t.Fatal("compact_at=0 must disable compaction")
	}
}

func TestMicroBeforeTurn(t *testing.T) {
	// Enough tool output to exceed the threshold, but few turns: the
	// micro level alone must bring it under.
	msgs := msgsOf(4, 12000)                                       // ~12k bytes of tool output per turn
	res := Compact(msgs, Options{ContextTokens: 8000, Ratio: 0.7}) // budget 5600
	if !res.Changed {
		t.Fatal("expected compaction")
	}
	if res.Level != "micro" {
		t.Fatalf("level = %s, want micro first", res.Level)
	}
	// Turn structure intact.
	if len(res.Messages) != len(msgs) {
		t.Fatalf("micro must not drop messages: %d -> %d", len(msgs), len(res.Messages))
	}
	// Placeholders carry the tool name and original length.
	found := false
	for _, m := range res.Messages {
		if strings.HasPrefix(m.Content, "[旧工具输出已压缩]") {
			if !strings.Contains(m.Content, "run_shell") || !strings.Contains(m.Content, "原长=") {
				t.Fatalf("placeholder missing fields: %s", m.Content)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no placeholder produced")
	}
	if res.After >= res.Before {
		t.Fatalf("estimate did not shrink: %d -> %d", res.Before, res.After)
	}
}

func TestTurnFoldWhenMicroNotEnough(t *testing.T) {
	// Five turns of text with no big tool outputs: micro cannot help.
	var msgs []model.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			model.Message{Role: model.RoleUser, Content: "user line " + itoa(i) + " " + strings.Repeat("a", 400)},
			model.Message{Role: model.RoleAssistant, Content: "reply " + itoa(i) + " " + strings.Repeat("b", 400)},
		)
	}
	res := Compact(msgs, Options{ContextTokens: 1000, Ratio: 0.7}) // budget 700
	if !res.Changed || res.Level != "turn" {
		t.Fatalf("level = %q, want turn", res.Level)
	}
	if !isSummary(res.Messages[0]) {
		t.Fatalf("first message must be the summary block: %q", res.Messages[0].Content)
	}
	// The in-flight (last) turn is untouched.
	if !strings.Contains(res.Messages[len(res.Messages)-1].Content, "reply 4") {
		t.Fatal("in-flight turn was folded")
	}
	// Summary carries user/assistant first lines and tool list.
	s := res.Messages[0].Content
	if !strings.Contains(s, "用户: user line 0") || !strings.Contains(s, "助手: reply 0") {
		t.Fatalf("summary content = %s", s)
	}
}

func TestToolPairsNeverSplit(t *testing.T) {
	msgs := msgsOf(6, 3000)
	res := Compact(msgs, Options{ContextTokens: 2000, Ratio: 0.7})
	// Every assistant tool_call must still be followed by its result
	// somewhere, OR both must be inside the folded range.
	pending := map[string]bool{}
	for i, m := range res.Messages {
		for _, tc := range m.ToolCalls {
			pending[tc.ID] = true
		}
		if m.Role == model.RoleTool && m.ToolCallID != "" {
			delete(pending, m.ToolCallID)
			if i > 0 {
				// No orphan result right after the folded summary
				// without its call: the summary replaces whole turns.
				_ = i
			}
		}
	}
	// Orphans must not exist outside the folded span.
	spanEnd := res.Span[1]
	for i, m := range res.Messages {
		if m.Role == model.RoleTool && m.ToolCallID != "" && pending[m.ToolCallID] {
			if i > 0 && res.Span != nil && i <= spanEnd {
				t.Fatalf("tool result %s inside folded range without its call", m.ToolCallID)
			}
		}
	}
	// Assert on the retained tail: no tool result whose call was folded.
	// The retained tail starts at a user message (no orphan tool result
	// before its own turn's user message).
	for i, m := range res.Messages {
		if m.Role == model.RoleTool && i > 0 {
			// Walk back to the nearest user message; the tool call for
			// this result must exist after it.
			ok := false
			for j := i - 1; j >= 0 && res.Messages[j].Role != model.RoleUser; j-- {
				for _, tc := range res.Messages[j].ToolCalls {
					if tc.ID == m.ToolCallID {
						ok = true
					}
				}
			}
			if !ok {
				t.Fatalf("orphan tool result without its call in the retained tail: %s", m.ToolCallID)
			}
		}
	}
}

func TestIncrementalSummaryStable(t *testing.T) {
	msgs := msgsOf(8, 2000)
	first := Compact(msgs, Options{ContextTokens: 900, Ratio: 0.7})
	if !first.Changed || first.Level != "turn" {
		t.Fatalf("first pass must reach the turn level: %s", first.Level)
	}
	firstSummary := first.Messages[0].Content

	// Append more turns, then compact again.
	more := append([]model.Message{}, first.Messages...)
	for i := 8; i < 12; i++ {
		more = append(more, buildTurn(i, 2000)...)
	}
	second := Compact(more, Options{ContextTokens: 900, Ratio: 0.7})
	if !second.Changed || second.Level != "turn" {
		t.Fatalf("second pass must reach the turn level: %s", second.Level)
	}
	// The old summary block survives byte-identical (prefix stability).
	if second.Messages[0].Content != firstSummary {
		t.Fatalf("existing summary rewritten:\nold=%q\nnew=%q", firstSummary, second.Messages[0].Content)
	}
	// A new block was appended after it.
	if !isSummary(second.Messages[1]) || second.Messages[1].Content == firstSummary {
		t.Fatalf("no new summary block after the old one")
	}
}

func TestSpillRefsPreserved(t *testing.T) {
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "big output task"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "tc1", Type: "function",
			Function: model.Function{Name: "run_shell", Arguments: "{}"}}}},
		{Role: model.RoleTool, Name: "run_shell", ToolCallID: "tc1",
			Content: "truncated head...\n[full output: spill:abc123]\n" + strings.Repeat("z", 3000)},
		{Role: model.RoleAssistant, Content: "done"},
		{Role: model.RoleUser, Content: "second task"},
		{Role: model.RoleAssistant, Content: "second reply"},
	}
	res := Compact(msgs, Options{ContextTokens: 1000, Ratio: 0.7})
	var joined []string
	for _, m := range res.Messages {
		joined = append(joined, m.Content)
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "spill:abc123") {
		t.Fatalf("spill reference lost:\n%s", all)
	}
}

func TestRepeatedCompactions(t *testing.T) {
	msgs := msgsOf(4, 3000)
	cur := msgs
	folds := 0
	for i := 0; i < 6; i++ {
		res := Compact(cur, Options{ContextTokens: 900, Ratio: 0.7})
		if !res.Changed {
			break
		}
		folds++
		cur = res.Messages
		// Never grow unboundedly.
		if len(cur) > len(msgs) {
			t.Fatalf("history grew: %d > %d", len(cur), len(msgs))
		}
	}
	if folds == 0 {
		t.Fatal("no compaction happened")
	}
	// Summary blocks stay stable across passes.
	summaries := 0
	for _, m := range cur {
		if isSummary(m) {
			summaries++
		}
	}
	if summaries == 0 {
		t.Fatal("no summary blocks remain")
	}
}

func TestSpillRefsIn(t *testing.T) {
	refs := spillRefsIn("a spill:aaa b spill:bbb, spill:aaa)")
	if len(refs) != 2 || refs[0] != "spill:aaa" || refs[1] != "spill:bbb" {
		t.Fatalf("refs = %v", refs)
	}
}

// ---- M7 tests: record persistence and resume rebuild ----

func TestRecordRoundTripAndApply(t *testing.T) {
	m := New(t.TempDir(), "sess-1")
	msgs := msgsOf(6, 3000)
	res := Compact(msgs, Options{ContextTokens: 900, Ratio: 0.7})
	if !res.Changed || len(res.NewRecords) == 0 {
		t.Fatal("expected a record")
	}
	for _, r := range res.NewRecords {
		if err := m.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	recs, warn, err := m.Load()
	if err != nil || warn != "" {
		t.Fatalf("load: %v %q", err, warn)
	}
	if len(recs) != len(res.NewRecords) {
		t.Fatalf("records = %d, want %d", len(recs), len(res.NewRecords))
	}
	// Rebuild from the FULL history + records must equal the in-memory
	// compacted array message for message.
	rebuilt, warn := ApplyRecords(msgs, recs)
	if warn != "" {
		t.Fatalf("apply warning: %s", warn)
	}
	if len(rebuilt) != len(res.Messages) {
		t.Fatalf("rebuilt len = %d, in-memory %d", len(rebuilt), len(res.Messages))
	}
	for i := range rebuilt {
		if rebuilt[i].Role != res.Messages[i].Role || rebuilt[i].Content != res.Messages[i].Content {
			t.Fatalf("message %d differs:\nrebuilt=%+v\nmemory=%+v", i, rebuilt[i], res.Messages[i])
		}
	}
}

func TestTornRecordFallsBack(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "sess-torn")
	// Write a valid record then a torn half-line.
	msgs := msgsOf(6, 3000)
	res := Compact(msgs, Options{ContextTokens: 900, Ratio: 0.7})
	if err := m.Append(res.NewRecords[0]); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "sess-torn.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"ts":"x","level":"turn","span":[0,`)
	f.Close()

	recs, warn, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("torn tail must produce a warning")
	}
	if len(recs) != 1 {
		t.Fatalf("valid records kept = %d, want 1", len(recs))
	}
	// Missing file: no records, no error, no warning.
	missing := New(dir, "sess-missing")
	if recs, warn, err := missing.Load(); err != nil || warn != "" || recs != nil {
		t.Fatalf("missing file = %v %q %v", recs, warn, err)
	}
	// A malformed span degrades to the uncompacted history.
	rebuilt, warn := ApplyRecords(msgs, []Record{{Level: "turn", Span: []int{0, 9999}, Summary: "x"}})
	if warn == "" || len(rebuilt) != len(msgs) {
		t.Fatal("out-of-range record must fall back to the uncompacted history")
	}
}

func TestConcurrentRecordAppends(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "sess-race")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.Append(Record{Level: "micro", Span: []int{i, i}, Kept: i})
		}(i)
	}
	wg.Wait()
	recs, warn, err := m.Load()
	if err != nil || warn != "" {
		t.Fatalf("load: %v %q", err, warn)
	}
	if len(recs) != 20 {
		t.Fatalf("records = %d, want 20 (no torn lines under concurrency)", len(recs))
	}
}
