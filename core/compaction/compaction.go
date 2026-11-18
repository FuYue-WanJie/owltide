// Package compaction implements context compaction (P2b-M6/M7).
//
// Design (docs/design/p2b.md §3.1):
//
//   - The sessionlog stays the single source of truth. Compaction never
//     rewrites it; it only changes the in-memory message array sent to
//     the model.
//   - Records are append-only lines in
//     ~/.owltide/compaction/<session>.jsonl (one Record per fold).
//   - Two levels, light first:
//     1. micro: replace old, large tool outputs of COMPLETED turns with
//     a placeholder (tool name + argument digest + original length +
//     spill reference).
//     2. turn: fold the oldest completed turns (user message, tool
//     pairs, final reply) into one templated summary message.
//   - Folding happens ONLY at turn boundaries: a tool_call is never
//     separated from its tool_result, the in-flight turn and the stable
//     prefix are never touched.
//   - Incremental: each fold appends a new summary block; previous
//     summaries are never rewritten or merged, so the already-folded
//     prefix stays byte-identical (cache friendly).
//   - Template summaries: user first line + final reply first line +
//     tool-name list, plus preserved spill references. No model calls.
//   - Threshold: 0.7 of the resolved context window (config
//     budget.compact_at; 0 disables). The denominator is
//     context_tokens ONLY — never budget.max_tokens.
package compaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FuYue-WanJie/owltide/core/model"
)

// Record is one fold, as persisted (p2b.md §3.1).
type Record struct {
	TS      string `json:"ts"`
	Level   string `json:"level"` // "micro" | "turn"
	Span    []int  `json:"span"`  // folded/summarized message indices (inclusive)
	Summary string `json:"summary,omitempty"`
	Kept    int    `json:"kept"` // messages kept after the fold
	// Before/After are rough token estimates (payload text only).
	Before int `json:"before,omitempty"`
	After  int `json:"after,omitempty"`
}

// DefaultAt is the default compact_at ratio (p2b.md §3.5 Q9: A).
const DefaultAt = 0.7

// SummaryLimit caps one template summary (bytes).
const SummaryLimit = 1200

// PlaceholderLimit is the max length of a micro placeholder.
const PlaceholderLimit = 400

// Manager persists and reads records for one session.
type Manager struct {
	Dir       string // ~/.owltide/compaction
	SessionID string
	mu        sync.Mutex
}

// New builds a manager for one session.
func New(dir, sessionID string) *Manager {
	return &Manager{Dir: dir, SessionID: sessionID}
}

func (m *Manager) path() string {
	return filepath.Join(m.Dir, m.SessionID+".jsonl")
}

// Append writes one record (append-only, one JSON line).
func (m *Manager) Append(r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(m.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Load returns all records in order. A missing file yields (nil, nil);
// a torn final line is dropped with a warning (crash safety).
func (m *Manager) Load() (records []Record, warning string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.path())
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			warning = fmt.Sprintf("compaction: dropped torn record at line %d", i+1)
			break
		}
		records = append(records, r)
	}
	return records, warning, nil
}

// TokenEstimate is a cheap approximate token count (4 bytes/token;
// deliberately coarse — used only for threshold decisions).
func TokenEstimate(msgs []model.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + len(m.Name)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n / 4
}

// CompactAt resolves the ratio: 0 disables; unset (negative) means
// DefaultAt.
func CompactAt(configured float64) float64 {
	if configured < 0 {
		return DefaultAt
	}
	if configured > 1 {
		return 1
	}
	return configured
}

// Threshold reports the token budget for compaction given the resolved
// context window and the ratio. A zero ratio or window means off.
func Threshold(contextTokens int, ratio float64) int {
	if contextTokens <= 0 || ratio <= 0 {
		return 0
	}
	return int(float64(contextTokens) * ratio)
}

// ---- the transform itself ----

// Options for one compaction pass.
type Options struct {
	// ContextTokens is the resolved model context window (the ONLY
	// denominator; budget.max_tokens never participates).
	ContextTokens int
	// Ratio is budget.compact_at (0 disables).
	Ratio float64
	// Blob returns the full tool output for a spill reference (used to
	// build placeholders and to verify references). May be nil.
	Blob func(ref string) (string, error)
}

// Result describes what one pass did.
type Result struct {
	Changed bool
	Level   string // "micro" | "turn" | ""
	Span    []int
	Kept    int
	Before  int
	After   int
	// Messages is the compacted array (records applied) to install as
	// the agent history.
	Messages []model.Message
	// NewRecords are the records to persist (in order).
	NewRecords []Record
	// microTouched are absolute indices of tool results replaced by
	// the micro level (contiguous spans recorded as records).
	microTouched []int
}

// boundary classifies the message array into stable units: a user
// message opens a turn; everything until the next user message
// belongs to it. Tool pairs stay inside their turn.
type turn struct {
	start, end int // [start, end)
}

func turnsOf(msgs []model.Message) []turn {
	var out []turn
	for i, m := range msgs {
		if m.Role == model.RoleUser {
			if len(out) > 0 && out[len(out)-1].end == 0 {
				out[len(out)-1].end = i
			}
			out = append(out, turn{start: i})
		}
	}
	if len(out) > 0 {
		out[len(out)-1].end = len(msgs)
	}
	return out
}

// keepRecent is the number of trailing turns never folded (the
// in-flight turn is always excluded by the caller).
const keepRecent = 1

// summaries are the folded blocks already installed (the last message
// of previous folds). They are skipped, never re-summarized.
func isSummary(m model.Message) bool {
	return m.Role == model.RoleUser && strings.HasPrefix(m.Content, summaryMark)
}

const summaryMark = "[已折叠的早期对话 — 摘要]"

// Compact performs at most one light pass and one heavy pass per call,
// in that order (p2b.md §3.1: light first).
func Compact(msgs []model.Message, opt Options) Result {
	ratio := CompactAt(opt.Ratio)
	budget := Threshold(opt.ContextTokens, ratio)
	if budget <= 0 || len(msgs) == 0 {
		return Result{Messages: msgs}
	}

	// Work on a copy; the in-flight turn (last turn) is never touched.
	work := append([]model.Message(nil), msgs...)
	before := TokenEstimate(work)
	turns := turnsOf(work)

	res := Result{Messages: work, Before: before, Kept: len(work)}

	// Level 1: micro — replace old large tool results of completed
	// turns with placeholders. Only when over the threshold.
	if TokenEstimate(work) > budget {
		if touched := microCompact(work, turns); len(touched) > 0 {
			res.Changed = true
			res.Level = "micro"
			res.Messages = work
			res.microTouched = touched
		}
	}
	// Under the threshold after micro (or nothing replaced): done.
	over := TokenEstimate(work) > budget
	if !over {
		if res.Changed {
			res.After = TokenEstimate(work)
			res.Kept = len(work)
			spans := coalesce(res.microTouched)
			for _, sp := range spans {
				res.NewRecords = append(res.NewRecords, Record{
					Level: "micro", Span: sp, Kept: len(work),
					Before: before, After: res.After,
				})
			}
			if len(spans) > 0 {
				res.Span = spans[len(spans)-1]
			}
		}
		return res
	}

	// Level 2: turn — fold the oldest completed turns into one
	// template summary, appended after any existing summary block.
	// Candidate turns: completed (never the in-flight one). Existing
	// summary blocks are preserved ahead of the fold, never
	// re-summarized.
	var candidates []int
	var prevSummary []model.Message
	for i, t := range turns {
		if i == len(turns)-1 {
			break // in-flight turn — never folded
		}
		if isSummary(work[t.start]) {
			prevSummary = append(prevSummary, work[t.start])
			continue // already folded content
		}
		candidates = append(candidates, i)
	}
	// Keep the newest keepRecent completed turns unfolded.
	foldCount := len(candidates) - keepRecent
	if foldCount <= 0 {
		return res // nothing safe to fold
	}
	last := candidates[foldCount] // index of the first kept turn
	start := candidates[0]        // first turn to fold
	end := turns[last].start
	// Never split a tool pair: the boundary is a user-message start.
	for end < len(work) && work[end].Role != model.RoleUser {
		end++
	}
	if end <= 0 {
		return res
	}

	var toolNames []string
	var spillRefs []string
	var sb strings.Builder
	appendLine := func(s string) {
		if s == "" {
			return
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(s)
	}
	for i := start; i < end; i++ {
		m := work[i]
		switch m.Role {
		case model.RoleUser:
			appendLine("- 用户: " + firstLine(m.Content))
		case model.RoleTool:
			toolNames = append(toolNames, m.Name)
			for _, ref := range spillRefsIn(m.Content) {
				if !containsStr(spillRefs, ref) {
					spillRefs = append(spillRefs, ref)
				}
			}
		case model.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					toolNames = appendUnique(toolNames, tc.Function.Name)
				}
				continue
			}
			if strings.TrimSpace(m.Content) != "" {
				appendLine("- 助手: " + firstLine(m.Content))
			}
		}
	}
	if len(toolNames) > 0 {
		appendLine("- 工具: " + strings.Join(toolNames, ", "))
	}
	if len(spillRefs) > 0 {
		appendLine("- spill 引用: " + strings.Join(spillRefs, ", "))
	}
	summary := summaryMark + "\n" + sb.String() + "\n（用 context_expand 取回原文，context_search 检索）"
	if len(summary) > SummaryLimit {
		// Truncate at a line boundary inside the limit.
		cut := summary[:SummaryLimit]
		if i := strings.LastIndex(cut, "\n"); i > 0 {
			cut = cut[:i]
		}
		summary = cut + "\n（摘要已截断；用 context_expand 取回原文）"
	}

	// Previous summary blocks stay byte-identical in front (prefix
	// stability), the new block is appended after them.
	kept := append([]model.Message{}, prevSummary...)
	kept = append(kept, model.Message{Role: model.RoleUser, Content: summary})
	kept = append(kept, work[end:]...)

	// Micro placeholders applied to survivors persist independently of
	// the turn fold (absolute spans).
	for _, sp := range coalesce(res.microTouched) {
		res.NewRecords = append(res.NewRecords, Record{
			Level: "micro", Span: sp, Kept: len(kept),
			Before: before, After: TokenEstimate(kept),
		})
	}
	res.Changed = true
	res.Level = "turn"
	res.Messages = kept
	res.Kept = len(kept)
	res.Span = []int{start, end - 1}
	res.After = TokenEstimate(kept)
	res.NewRecords = append(res.NewRecords, Record{
		Level: "turn", Span: res.Span, Summary: summary, Kept: len(kept),
		Before: before, After: res.After,
	})
	return res
}

// microCompact replaces old large tool results in COMPLETED turns.
// Returns the number replaced.
func microCompact(msgs []model.Message, turns []turn) []int {
	const large = 600 // bytes
	var touched []int
	for ti, t := range turns {
		if ti == len(turns)-1 {
			break // in-flight
		}
		for i := t.start; i < t.end; i++ {
			m := &msgs[i]
			if m.Role != model.RoleTool || len(m.Content) <= large {
				continue
			}
			orig := len(m.Content)
			argsDigest := ""
			// Find the matching tool_call for the argument digest.
			for j := i - 1; j >= t.start; j-- {
				for _, tc := range msgs[j].ToolCalls {
					if tc.ID == m.ToolCallID {
						sum := sha256.Sum256([]byte(tc.Function.Arguments))
						argsDigest = hex.EncodeToString(sum[:4])
					}
				}
			}
			refs := spillRefsIn(m.Content)
			ph := fmt.Sprintf("[旧工具输出已压缩] tool=%s args=%s 原长=%dB", m.Name, argsDigest, orig)
			if len(refs) > 0 {
				ph += " spill=" + strings.Join(refs, ",")
			}
			if len(ph) > PlaceholderLimit {
				ph = ph[:PlaceholderLimit]
			}
			m.Content = ph
			m.ToolCalls = nil
			touched = append(touched, i)
		}
	}
	return touched
}

// coalesce turns sorted indices into contiguous inclusive spans.
func coalesce(ids []int) [][]int {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]int(nil), ids...)
	sort.Ints(sorted)
	var out [][]int
	lo, hi := sorted[0], sorted[0]
	for _, v := range sorted[1:] {
		if v == hi+1 {
			hi = v
			continue
		}
		out = append(out, []int{lo, hi})
		lo, hi = v, v
	}
	out = append(out, []int{lo, hi})
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// spillRefsIn extracts spill: references from a text.
func spillRefsIn(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "spill:")
		if i < 0 {
			break
		}
		rest := s[i:]
		end := len(rest)
		for k, r := range rest {
			if r == ' ' || r == '\n' || r == ')' || r == ',' || r == ']' || r == '}' {
				end = k
				break
			}
		}
		ref := rest[:end]
		if len(ref) > len("spill:") {
			if !containsStr(out, ref) {
				out = append(out, ref)
			}
		}
		s = s[i+end:]
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func appendUnique(list []string, s string) []string {
	if containsStr(list, s) {
		return list
	}
	return append(list, s)
}

// ApplyRecords rebuilds a compacted history from the full sessionlog
// history and the persisted records (M7).
//
// Spans are ABSOLUTE indices into the full (uncompacted) history, so
// reconstruction never accumulates shifted indices: micro spans mark
// tool-result ranges to placeholder, turn spans mark ranges replaced
// by a summary block. Later turn records are applied first-class (their
// spans start after every earlier fold), and an existing summary block
// is preserved verbatim because it lies before any later turn span.
//
// A missing/empty record list yields the full history. Malformed or
// out-of-range records degrade to the FULL uncompacted history with a
// warning (never a half-applied state).
func ApplyRecords(full []model.Message, recs []Record) ([]model.Message, string) {
	if len(recs) == 0 {
		return full, ""
	}
	// 1. Validate every record against the FULL history first.
	for _, r := range recs {
		if len(r.Span) != 2 {
			return full, "compaction: malformed span; using uncompacted history"
		}
		lo, hi := r.Span[0], r.Span[1]
		if lo < 0 || hi >= len(full) || lo > hi {
			return full, "compaction: span out of range; using uncompacted history"
		}
		switch r.Level {
		case "micro":
		case "turn":
			if r.Summary == "" {
				return full, "compaction: empty turn summary; using uncompacted history"
			}
		default:
			return full, "compaction: unknown record level; using uncompacted history"
		}
	}

	// 2. Collect turn spans (they partition the foldable front) and
	// collect micro ranges (they decorate results that survive).
	var turnRecs []Record
	for _, r := range recs {
		if r.Level == "turn" {
			turnRecs = append(turnRecs, r)
		}
	}
	sort.Slice(turnRecs, func(i, j int) bool { return turnRecs[i].Span[0] < turnRecs[j].Span[0] })

	// 3. Build: everything before the first turn fold, then one summary
	// block per turn record (in fold order), then the retained tail.
	var out []model.Message
	cursor := 0
	for _, tr := range turnRecs {
		lo, hi := tr.Span[0], tr.Span[1]
		if lo < cursor {
			return full, "compaction: overlapping turn spans; using uncompacted history"
		}
		out = append(out, full[cursor:lo]...)
		out = append(out, model.Message{Role: model.RoleUser, Content: tr.Summary})
		cursor = hi + 1
	}
	out = append(out, full[cursor:]...)

	// 4. Apply micro placeholders by absolute span on the FULL history,
	// then map them onto surviving messages by index identity.
	placeholder := map[int]string{}
	for _, r := range recs {
		if r.Level != "micro" {
			continue
		}
		for i := r.Span[0]; i <= r.Span[1] && i < len(full); i++ {
			m := full[i]
			if m.Role != model.RoleTool || len(m.Content) <= microLargeBytes {
				continue
			}
			placeholder[i] = microPlaceholder(m, i, full)
		}
	}
	if len(placeholder) > 0 {
		// Survivor indices: positions in `out` that correspond to FULL
		// indices. Walk the same partition used above.
		survivor := map[int]int{} // out index -> full index (or -1)
		fullIdx := 0
		turnIdx := 0
		for oi := 0; oi < len(out); oi++ {
			if turnIdx < len(turnRecs) && fullIdx == turnRecs[turnIdx].Span[0] {
				// summary block occupies this slot
				survivor[oi] = -1
				fullIdx = turnRecs[turnIdx].Span[1] + 1
				turnIdx++
				continue
			}
			survivor[oi] = fullIdx
			fullIdx++
		}
		for oi := range out {
			fi, ok := survivor[oi]
			if !ok || fi < 0 {
				continue
			}
			if ph, has := placeholder[fi]; has {
				out[oi].Content = ph
				out[oi].ToolCalls = nil
			}
		}
	}
	return out, ""
}

const microLargeBytes = 600

// microPlaceholder builds the placeholder text for one tool result.
func microPlaceholder(m model.Message, idx int, full []model.Message) string {
	orig := len(m.Content)
	// Recover the argument digest from the matching tool_call (same
	// text as the live path, so rebuilds are byte-identical).
	argsDigest := ""
	for j := idx - 1; j >= 0; j-- {
		if full[j].Role == model.RoleUser {
			break
		}
		for _, tc := range full[j].ToolCalls {
			if tc.ID == m.ToolCallID {
				sum := sha256.Sum256([]byte(tc.Function.Arguments))
				argsDigest = hex.EncodeToString(sum[:4])
			}
		}
	}
	ph := fmt.Sprintf("[旧工具输出已压缩] tool=%s args=%s 原长=%dB", m.Name, argsDigest, orig)
	if refs := spillRefsIn(m.Content); len(refs) > 0 {
		ph += " spill=" + strings.Join(refs, ",")
	}
	if len(ph) > PlaceholderLimit {
		ph = ph[:PlaceholderLimit]
	}
	return ph
}

// microCompactRange applies the micro transform to msgs[lo:hi].
func microCompactRange(msgs []model.Message, lo, hi int) {
	const large = 600
	for i := lo; i < hi && i < len(msgs); i++ {
		m := &msgs[i]
		if m.Role != model.RoleTool || len(m.Content) <= large {
			continue
		}
		if strings.HasPrefix(m.Content, "[旧工具输出已压缩]") {
			continue
		}
		orig := len(m.Content)
		refs := spillRefsIn(m.Content)
		ph := fmt.Sprintf("[旧工具输出已压缩] tool=%s 原长=%dB", m.Name, orig)
		if len(refs) > 0 {
			ph += " spill=" + strings.Join(refs, ",")
		}
		if len(ph) > PlaceholderLimit {
			ph = ph[:PlaceholderLimit]
		}
		m.Content = ph
		m.ToolCalls = nil
	}
}
