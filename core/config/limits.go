package config

// Model parameter matching (P2b M11/M12): resolve per-model limits
// with a four-level, per-field fallback:
//
//	1. user config        model.limits.*
//	2. endpoint /models   GET <base_url>/models (cached by model name)
//	3. builtin table      small, prefix-matched, official-doc numbers only
//	4. conservative       context 32768 / output 4096 / images=false /
//	                      tools=true / audio=false / video=false
//
// Every field falls back independently: a config that only sets
// context_tokens still lets the other fields fall through.
//
// Constraints (user decisions, docs/design/p2b.md §5):
//   - probing is ON by default, cached by model name, short timeout,
//     silent fallback on failure;
//   - probe results and endpoint addresses are NEVER logged or
//     committed (no decisions events, no startup dump of payloads);
//   - the builtin table stays small and only carries numbers traceable
//     to official documentation, each entry with source and date.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limits are the resolved model parameters used by the runtime.
type Limits struct {
	ContextTokens int  `json:"context_tokens"`
	MaxOutput     int  `json:"max_output"`
	Images        bool `json:"images"`
	Tools         bool `json:"tools"`
	Audio         bool `json:"audio"`
	Video         bool `json:"video"`
}

// LimitsConfig is the raw user configuration. Pointers distinguish
// "unset" from a legitimate zero/false so each field can fall back on
// its own.
type LimitsConfig struct {
	ContextTokens *int  `yaml:"context_tokens,omitempty"`
	MaxOutput     *int  `yaml:"max_output,omitempty"`
	Images        *bool `yaml:"images,omitempty"`
	Tools         *bool `yaml:"tools,omitempty"`
	Audio         *bool `yaml:"audio,omitempty"`
	Video         *bool `yaml:"video,omitempty"`
}

// Source names where one field's value came from.
type Source string

const (
	SourceConfig       Source = "config"
	SourceEndpoint     Source = "endpoint"
	SourceBuiltinTable Source = "builtin"
	SourceDefault      Source = "default"
)

// Defaults are the level-4 conservative values.
var Defaults = Limits{
	ContextTokens: 32768,
	MaxOutput:     4096,
	Images:        false,
	Tools:         true,
	Audio:         false,
	Video:         false,
}

// Resolved is the outcome of resolution: one value per field plus the
// source level that supplied it.
type Resolved struct {
	Limits  Limits
	Sources map[string]Source // field name -> source
}

// Field order is stable for display and tests.
var limitFields = []string{"context_tokens", "max_output", "images", "tools", "audio", "video"}

// partial is a sparse set of known fields (probe / builtin entries).
type partial struct {
	contextTokens *int
	maxOutput     *int
	images        *bool
	tools         *bool
	audio         *bool
	video         *bool
}

func (p partial) fill(r *Resolved, src Source) {
	if p.contextTokens != nil && r.Sources["context_tokens"] == "" {
		r.Limits.ContextTokens = *p.contextTokens
		r.Sources["context_tokens"] = src
	}
	if p.maxOutput != nil && r.Sources["max_output"] == "" {
		r.Limits.MaxOutput = *p.maxOutput
		r.Sources["max_output"] = src
	}
	if p.images != nil && r.Sources["images"] == "" {
		r.Limits.Images = *p.images
		r.Sources["images"] = src
	}
	if p.tools != nil && r.Sources["tools"] == "" {
		r.Limits.Tools = *p.tools
		r.Sources["tools"] = src
	}
	if p.audio != nil && r.Sources["audio"] == "" {
		r.Limits.Audio = *p.audio
		r.Sources["audio"] = src
	}
	if p.video != nil && r.Sources["video"] == "" {
		r.Limits.Video = *p.video
		r.Sources["video"] = src
	}
}

// builtinEntry is one row of the level-3 table. Numbers must be
// traceable to official vendor documentation; entries whose numbers
// cannot be cited are deliberately absent.
type builtinEntry struct {
	prefix  string
	limits  partial
	source  string // official doc reference (short form)
	entered string // date the numbers were recorded
}

// builtinTable is deliberately small. Prefix match takes the LONGEST
// matching prefix, so "gpt-4o-mini" beats "gpt-4o".
var builtinTable = []builtinEntry{
	{prefix: "gpt-4o-mini", source: "OpenAI models docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(128000), maxOutput: ip(16384), images: bp(true), tools: bp(true)}},
	{prefix: "gpt-4o", source: "OpenAI models docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(128000), maxOutput: ip(16384), images: bp(true), tools: bp(true)}},
	{prefix: "gpt-4.1", source: "OpenAI models docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(1047576), maxOutput: ip(32768), images: bp(true), tools: bp(true)}},
	{prefix: "claude-3-5-sonnet", source: "Anthropic models docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(200000), maxOutput: ip(8192), images: bp(true), tools: bp(true)}},
	{prefix: "claude-sonnet-4", source: "Anthropic models docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(200000), maxOutput: ip(32000), images: bp(true), tools: bp(true)}},
	{prefix: "deepseek-chat", source: "DeepSeek API docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(65536), maxOutput: ip(8192), images: bp(false), tools: bp(true)}},
	{prefix: "deepseek-reasoner", source: "DeepSeek API docs", entered: "2026-11-10",
		limits: partial{contextTokens: ip(65536), maxOutput: ip(8192), images: bp(false), tools: bp(true)}},
}

// BuiltinMatch returns the longest matching builtin entry for a model
// name (case-insensitive), and whether one matched.
func BuiltinMatch(model string) (partial, string, string, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	best := -1
	var hit builtinEntry
	for _, e := range builtinTable {
		if strings.HasPrefix(name, e.prefix) && len(e.prefix) > best {
			best = len(e.prefix)
			hit = e
		}
	}
	if best < 0 {
		return partial{}, "", "", false
	}
	return hit.limits, hit.source, hit.entered, true
}

// ---- endpoint probe ----

// ProbeTimeout bounds the /models request; it runs once per model name
// per process and must never hold up startup. Var (not const) so tests
// can shorten it.
var ProbeTimeout = 3 * time.Second

// probeCache caches endpoint probe outcomes by model name, including
// failures (negative cache) so a dead endpoint is not retried on every
// resolution.
var probeCache = struct {
	sync.Mutex
	m map[string]probeResult
}{m: map[string]probeResult{}}

type probeResult struct {
	limits partial
	ok     bool
}

// probeModels asks GET <baseURL>/models for metadata of the given
// model. Any parseable numeric field is used; unknown shapes are
// ignored. The endpoint address and payload are never logged.
func probeModels(baseURL, apiKey, model string) (partial, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	probeCache.Lock()
	if r, ok := probeCache.m[key]; ok {
		probeCache.Unlock()
		return r.limits, r.ok
	}
	probeCache.Unlock()

	res, ok := doProbe(baseURL, apiKey, model)

	probeCache.Lock()
	probeCache.m[key] = probeResult{limits: res, ok: ok}
	probeCache.Unlock()
	return res, ok
}

// resetProbeCache clears the process cache (tests).
func resetProbeCache() {
	probeCache.Lock()
	probeCache.m = map[string]probeResult{}
	probeCache.Unlock()
}

func doProbe(baseURL, apiKey, model string) (partial, bool) {
	if baseURL == "" {
		return partial{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return partial{}, false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return partial{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return partial{}, false
	}
	var doc struct {
		Data   []map[string]json.RawMessage `json:"data"`
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return partial{}, false
	}
	entries := doc.Data
	if len(entries) == 0 {
		entries = doc.Models
	}
	for _, e := range entries {
		id := rawString(e["id"])
		if id == "" || (strings.EqualFold(id, model) || id == model) {
			return parseProbeEntry(e), true
		}
	}
	// No id match: an endpoint with a single entry still describes
	// the configured model.
	if len(entries) == 1 {
		return parseProbeEntry(entries[0]), true
	}
	return partial{}, false
}

// parseProbeEntry reads the numeric fields OpenAI-compatible endpoints
// variously return. Unknown keys are ignored.
func parseProbeEntry(e map[string]json.RawMessage) partial {
	var p partial
	if v, ok := rawInt(e["context_window"]); ok {
		p.contextTokens = &v
	} else if v, ok := rawInt(e["context_length"]); ok {
		p.contextTokens = &v
	} else if v, ok := rawInt(e["max_context_length"]); ok {
		p.contextTokens = &v
	}
	if v, ok := rawInt(e["max_output"]); ok {
		p.maxOutput = &v
	} else if v, ok := rawInt(e["max_output_tokens"]); ok {
		p.maxOutput = &v
	} else if v, ok := rawInt(e["max_completion_tokens"]); ok {
		p.maxOutput = &v
	}
	if v, ok := rawBool(e["supports_images"]); ok {
		p.images = &v
	} else if v, ok := rawBool(e["images"]); ok {
		p.images = &v
	}
	if v, ok := rawBool(e["supports_tools"]); ok {
		p.tools = &v
	} else if v, ok := rawBool(e["tools"]); ok {
		p.tools = &v
	}
	if v, ok := rawBool(e["supports_audio"]); ok {
		p.audio = &v
	} else if v, ok := rawBool(e["audio"]); ok {
		p.audio = &v
	}
	if v, ok := rawBool(e["supports_video"]); ok {
		p.video = &v
	} else if v, ok := rawBool(e["video"]); ok {
		p.video = &v
	}
	return p
}

func rawString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return ""
	}
	return s
}

func rawInt(r json.RawMessage) (int, bool) {
	if len(r) == 0 {
		return 0, false
	}
	var v int
	if json.Unmarshal(r, &v) != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func rawBool(r json.RawMessage) (bool, bool) {
	if len(r) == 0 {
		return false, false
	}
	var v bool
	if json.Unmarshal(r, &v) != nil {
		return false, false
	}
	return v, true
}

// ---- resolution ----

// ResolveOptions controls resolution.
type ResolveOptions struct {
	// ProbeEndpoint enables the level-2 /models probe (default on;
	// callers disable it when no API key is configured).
	ProbeEndpoint bool
	// APIKey is sent only to the configured endpoint's /models.
	APIKey string
}

// ResolveLimits performs the four-level, per-field fallback.
func ResolveLimits(m Model, opt ResolveOptions) Resolved {
	r := Resolved{Sources: map[string]Source{}}

	// Level 1: user config.
	u := m.Limits
	if u.ContextTokens != nil {
		r.Limits.ContextTokens = *u.ContextTokens
		r.Sources["context_tokens"] = SourceConfig
	}
	if u.MaxOutput != nil {
		r.Limits.MaxOutput = *u.MaxOutput
		r.Sources["max_output"] = SourceConfig
	}
	if u.Images != nil {
		r.Limits.Images = *u.Images
		r.Sources["images"] = SourceConfig
	}
	if u.Tools != nil {
		r.Limits.Tools = *u.Tools
		r.Sources["tools"] = SourceConfig
	}
	if u.Audio != nil {
		r.Limits.Audio = *u.Audio
		r.Sources["audio"] = SourceConfig
	}
	if u.Video != nil {
		r.Limits.Video = *u.Video
		r.Sources["video"] = SourceConfig
	}

	// Level 2: endpoint probe (fills only fields still unset).
	if opt.ProbeEndpoint && m.Name != "" {
		if p, ok := probeModels(m.BaseURL, opt.APIKey, m.Name); ok {
			p.fill(&r, SourceEndpoint)
		}
	}

	// Level 3: builtin table (longest prefix match).
	if p, _, _, ok := BuiltinMatch(m.Name); ok {
		p.fill(&r, SourceBuiltinTable)
	}

	// Level 4: conservative defaults.
	for _, f := range limitFields {
		if r.Sources[f] == "" {
			r.Sources[f] = SourceDefault
			switch f {
			case "context_tokens":
				r.Limits.ContextTokens = Defaults.ContextTokens
			case "max_output":
				r.Limits.MaxOutput = Defaults.MaxOutput
			case "images":
				r.Limits.Images = Defaults.Images
			case "tools":
				r.Limits.Tools = Defaults.Tools
			case "audio":
				r.Limits.Audio = Defaults.Audio
			case "video":
				r.Limits.Video = Defaults.Video
			}
		}
	}
	return r
}

// FormatModelInfo renders the `owltide model info` table: one line per
// field with its resolved value and source level. It never prints the
// endpoint address, the API key, or probe payloads.
func FormatModelInfo(modelName string, r Resolved) string {
	var b strings.Builder
	fmt.Fprintf(&b, "model: %s\n", modelName)
	for _, f := range limitFields {
		var v string
		switch f {
		case "context_tokens":
			v = fmt.Sprintf("%d", r.Limits.ContextTokens)
		case "max_output":
			v = fmt.Sprintf("%d", r.Limits.MaxOutput)
		case "images":
			v = fmt.Sprintf("%t", r.Limits.Images)
		case "tools":
			v = fmt.Sprintf("%t", r.Limits.Tools)
		case "audio":
			v = fmt.Sprintf("%t", r.Limits.Audio)
		case "video":
			v = fmt.Sprintf("%t", r.Limits.Video)
		}
		src := r.Sources[f]
		fmt.Fprintf(&b, "%-14s %-6s (%s)\n", f+":", v, src)
	}
	return b.String()
}

// SortedFields returns the field names in display order (tests).
func SortedFields() []string {
	out := append([]string(nil), limitFields...)
	sort.Strings(out)
	return out
}

func ip(v int) *int   { return &v }
func bp(v bool) *bool { return &v }
