package agent

// P2b-M6: the reversible, read-only context tools. context_expand
// fetches original text from the sessionlog by span (paginated);
// context_search greps the session log. Both are read-only and safe in
// router mode (they only read the conversation).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

// ContextTools needs the session log path to read the full history.
type ContextTools struct {
	// LogPath is <sessions>/<id>/messages.jsonl.
	LogPath string
}

// ExpandPage bounds one context_expand response (bytes).
const ExpandPage = 8000

func (ContextTools) Caps() tool.Caps { return tool.CapReads }

func (t ContextTools) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "context_expand",
		Description: "Fetch the ORIGINAL text of a folded conversation span (from the " +
			"session log, not the compacted view). Span = message index range [from,to] " +
			"as shown in the summary. Paginated: returns up to " + strconv.Itoa(ExpandPage) + " bytes " +
			"plus next_offset. Read-only.",
		Parameters: json.RawMessage(`{"type":"object",` +
			`"properties":{"from":{"type":"integer"},"to":{"type":"integer"},` +
			`"next_offset":{"type":"integer","description":"byte offset within the span (0 first)"}},` +
			`"required":["from","to"]}`),
	}}
}

func (t ContextTools) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		From       int `json:"from"`
		To         int `json:"to"`
		NextOffset int `json:"next_offset"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	msgs, err := t.load()
	if err != nil {
		return "", err
	}
	if a.From < 0 || a.To >= len(msgs) || a.From > a.To {
		return "", fmt.Errorf("span [%d,%d] out of range (history has %d messages)", a.From, a.To, len(msgs))
	}
	var sb strings.Builder
	for i := a.From; i <= a.To; i++ {
		m := msgs[i]
		fmt.Fprintf(&sb, "#%d %s", i, m.Role)
		if m.Name != "" {
			sb.WriteString(" (" + m.Name + ")")
		}
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			sb.WriteString(" tool_call=" + tc.Function.Name + " " + tc.Function.Arguments)
		}
		sb.WriteString("\n")
	}
	text := sb.String()
	off := a.NextOffset
	if off < 0 {
		off = 0
	}
	if off >= len(text) {
		return fmt.Sprintf("span [%d,%d]: end of content (total %d bytes)", a.From, a.To, len(text)), nil
	}
	end := off + ExpandPage
	more := false
	if end < len(text) {
		more = true
	} else {
		end = len(text)
	}
	out := text[off:end]
	if more {
		return fmt.Sprintf("span [%d,%d] bytes [%d,%d) of %d — continue with next_offset=%d:\n%s",
			a.From, a.To, off, end, len(text), end, out), nil
	}
	return fmt.Sprintf("span [%d,%d] bytes [%d,%d) of %d (complete):\n%s",
		a.From, a.To, off, end, len(text), out), nil
}

func (t ContextTools) load() ([]model.Message, error) {
	if t.LogPath == "" {
		return nil, fmt.Errorf("context tools: no session log configured")
	}
	data, err := os.ReadFile(t.LogPath)
	if err != nil {
		return nil, err
	}
	var out []model.Message
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m model.Message
		if json.Unmarshal([]byte(line), &m) != nil {
			break // torn tail
		}
		out = append(out, m)
	}
	return out, nil
}

// ContextSearch greps the session log (the folded originals included).
type ContextSearch struct{ LogPath string }

func (ContextSearch) Caps() tool.Caps { return tool.CapReads }

func (ContextSearch) Schema() tool.Definition {
	return tool.Definition{Type: "function", Function: tool.FunctionSchema{
		Name: "context_search",
		Description: "Search the full session log (including folded content) for a " +
			"substring. Returns matching message indices with a short excerpt — use " +
			"context_expand to read the original. Read-only.",
		Parameters: json.RawMessage(`{"type":"object",` +
			`"properties":{"query":{"type":"string"}},` +
			`"required":["query"]}`),
	}}
}

// SearchLimit caps results per call.
const SearchLimit = 20

func (t ContextSearch) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return "", fmt.Errorf("query is required")
	}
	ct := ContextTools{LogPath: t.LogPath}
	msgs, err := ct.load()
	if err != nil {
		return "", err
	}
	var hits []string
	for i, m := range msgs {
		hay := m.Content
		for _, tc := range m.ToolCalls {
			hay += " " + tc.Function.Name + " " + tc.Function.Arguments
		}
		idx := strings.Index(hay, q)
		if idx < 0 {
			continue
		}
		lo := idx - 60
		if lo < 0 {
			lo = 0
		}
		hi := idx + len(q) + 60
		if hi > len(hay) {
			hi = len(hay)
		}
		excerpt := strings.ReplaceAll(hay[lo:hi], "\n", " ")
		hits = append(hits, fmt.Sprintf("#%d %s: …%s…", i, m.Role, excerpt))
		if len(hits) >= SearchLimit {
			break
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no match for %q in %d messages", q, len(msgs)), nil
	}
	return fmt.Sprintf("%d match(es) for %q in %d messages:\n%s",
		len(hits), q, len(msgs), strings.Join(hits, "\n")), nil
}
