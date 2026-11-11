package config

// Model parameter matching tests (P2b M11): per-field four-level
// fallback, probe timeout/bad JSON, cache hit, longest-prefix builtin
// match, and the no-address/no-key output invariant. All endpoints are
// httptest servers; no real model calls.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// probeServer starts a /models endpoint returning body (or sleeping
// first when delay > 0) and counts requests.
func probeServer(t *testing.T, body string, status int, delay time.Duration) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func modelWith(baseURL, name string) Model {
	return Model{Name: name, BaseURL: baseURL, APIKeyEnv: "OWLTIDE_API_KEY"}
}

// TestFieldWiseMixedFallback: every field falls back independently —
// config supplies context_tokens, the probe supplies max_output, the
// builtin table supplies images/tools, audio/video stay default.
func TestFieldWiseMixedFallback(t *testing.T) {
	resetProbeCache()
	srv, _ := probeServer(t, `{"data":[{"id":"gpt-4o-mini","max_output":9999}]}`, 200, 0)
	m := modelWith(srv.URL, "gpt-4o-mini")
	m.Limits.ContextTokens = ip(7777)

	r := ResolveLimits(m, ResolveOptions{ProbeEndpoint: true, APIKey: "k"})
	if r.Limits.ContextTokens != 7777 || r.Sources["context_tokens"] != SourceConfig {
		t.Fatalf("context: %d/%s", r.Limits.ContextTokens, r.Sources["context_tokens"])
	}
	if r.Limits.MaxOutput != 9999 || r.Sources["max_output"] != SourceEndpoint {
		t.Fatalf("max_output: %d/%s", r.Limits.MaxOutput, r.Sources["max_output"])
	}
	if !r.Limits.Images || r.Sources["images"] != SourceBuiltinTable {
		t.Fatalf("images: %v/%s", r.Limits.Images, r.Sources["images"])
	}
	if !r.Limits.Tools || r.Sources["tools"] != SourceBuiltinTable {
		t.Fatalf("tools: %v/%s", r.Limits.Tools, r.Sources["tools"])
	}
	if r.Limits.Audio || r.Sources["audio"] != SourceDefault {
		t.Fatalf("audio: %v/%s", r.Limits.Audio, r.Sources["audio"])
	}
	if r.Limits.Video || r.Sources["video"] != SourceDefault {
		t.Fatalf("video: %v/%s", r.Limits.Video, r.Sources["video"])
	}
}

// TestProbeTimeoutFallsBackSilently: a slow endpoint must not hold up
// resolution; fields fall through to builtin/default without error.
func TestProbeTimeoutFallsBackSilently(t *testing.T) {
	resetProbeCache()
	old := ProbeTimeout
	ProbeTimeout = 100 * time.Millisecond
	defer func() { ProbeTimeout = old }()

	srv, _ := probeServer(t, `{"data":[{"id":"deepseek-chat","max_output":1}]}`, 200, 400*time.Millisecond)
	m := modelWith(srv.URL, "deepseek-chat")
	r := ResolveLimits(m, ResolveOptions{ProbeEndpoint: true, APIKey: "k"})
	if r.Sources["max_output"] != SourceBuiltinTable || r.Limits.MaxOutput != 8192 {
		t.Fatalf("timeout must fall back to builtin: %d/%s", r.Limits.MaxOutput, r.Sources["max_output"])
	}
}

// TestProbeBadJSONFallsBack: an unparseable /models body is ignored.
func TestProbeBadJSONFallsBack(t *testing.T) {
	resetProbeCache()
	srv, _ := probeServer(t, `this is not json`, 200, 0)
	m := modelWith(srv.URL, "deepseek-chat")
	r := ResolveLimits(m, ResolveOptions{ProbeEndpoint: true, APIKey: "k"})
	if r.Sources["context_tokens"] != SourceBuiltinTable || r.Limits.ContextTokens != 65536 {
		t.Fatalf("bad json must fall back to builtin: %d/%s", r.Limits.ContextTokens, r.Sources["context_tokens"])
	}
}

// TestProbeNon200FallsBack: an error status is ignored.
func TestProbeNon200FallsBack(t *testing.T) {
	resetProbeCache()
	srv, _ := probeServer(t, `{"data":[]}`, 500, 0)
	m := modelWith(srv.URL, "gpt-4o")
	r := ResolveLimits(m, ResolveOptions{ProbeEndpoint: true, APIKey: "k"})
	if r.Sources["context_tokens"] != SourceBuiltinTable {
		t.Fatalf("status 500 must fall back to builtin, got %s", r.Sources["context_tokens"])
	}
}

// TestProbeCacheByModelName: resolution twice hits the endpoint once
// (positive cache), and the cached values still apply.
func TestProbeCacheByModelName(t *testing.T) {
	resetProbeCache()
	srv, hits := probeServer(t, `{"data":[{"id":"cached-model","context_window":4242}]}`, 200, 0)
	m := modelWith(srv.URL, "cached-model")
	for i := 0; i < 3; i++ {
		r := ResolveLimits(m, ResolveOptions{ProbeEndpoint: true, APIKey: "k"})
		if r.Limits.ContextTokens != 4242 || r.Sources["context_tokens"] != SourceEndpoint {
			t.Fatalf("round %d: %d/%s", i, r.Limits.ContextTokens, r.Sources["context_tokens"])
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("probe requests = %d, want 1 (cache by model name)", got)
	}
}

// TestProbeNegativeCache: a failed probe is cached too (no retry storm).
func TestProbeNegativeCache(t *testing.T) {
	resetProbeCache()
	srv, hits := probeServer(t, `bad`, 200, 0)
	m := modelWith(srv.URL, "neg-model")
	for i := 0; i < 3; i++ {
		ResolveLimits(m, ResolveOptions{ProbeEndpoint: true, APIKey: "k"})
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("failed probe requests = %d, want 1 (negative cache)", got)
	}
}

// TestBuiltinLongestPrefix: the longest matching prefix wins.
func TestBuiltinLongestPrefix(t *testing.T) {
	old := builtinTable
	builtinTable = append(append([]builtinEntry{}, old...),
		builtinEntry{prefix: "test-model", source: "test", entered: "2026-11-10",
			limits: partial{contextTokens: ip(111)}},
		builtinEntry{prefix: "test-model-mini", source: "test", entered: "2026-11-10",
			limits: partial{contextTokens: ip(222)}},
	)
	defer func() { builtinTable = old }()

	p, _, _, ok := BuiltinMatch("test-model-mini-2026-01")
	if !ok || p.contextTokens == nil || *p.contextTokens != 222 {
		t.Fatalf("longest prefix lost: %+v", p)
	}
	p, _, _, ok = BuiltinMatch("test-model-2026-01")
	if !ok || p.contextTokens == nil || *p.contextTokens != 111 {
		t.Fatalf("short prefix: %+v", p)
	}
	if _, _, _, ok := BuiltinMatch("unrelated"); ok {
		t.Fatal("unrelated model must not match")
	}
}

// TestConfigWinsAndProbeSwitch: explicit config wins for every field,
// and ProbeEndpoint=false issues no request at all.
func TestConfigWinsAndProbeSwitch(t *testing.T) {
	resetProbeCache()
	srv, hits := probeServer(t, `{"data":[{"id":"cfg-model","context_window":1}]}`, 200, 0)
	m := modelWith(srv.URL, "cfg-model")
	m.Limits = LimitsConfig{
		ContextTokens: ip(100), MaxOutput: ip(50),
		Images: bp(true), Tools: bp(false), Audio: bp(true), Video: bp(false),
	}
	r := ResolveLimits(m, ResolveOptions{ProbeEndpoint: false})
	for _, f := range limitFields {
		if r.Sources[f] != SourceConfig {
			t.Fatalf("field %s source = %s, want config", f, r.Sources[f])
		}
	}
	if r.Limits.Tools || !r.Limits.Audio || r.Limits.Video {
		t.Fatalf("bool config not honored: %+v", r.Limits)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("probe ran with ProbeEndpoint=false: %d requests", got)
	}
}

// TestDefaultsForUnknownModel: unknown model, probing off -> all four
// defaults, including the new audio/video capability flags.
func TestDefaultsForUnknownModel(t *testing.T) {
	r := ResolveLimits(modelWith("", "unknown-xyz"), ResolveOptions{})
	want := Defaults
	if r.Limits != want {
		t.Fatalf("limits = %+v, want %+v", r.Limits, want)
	}
	for _, f := range limitFields {
		if r.Sources[f] != SourceDefault {
			t.Fatalf("%s source = %s", f, r.Sources[f])
		}
	}
}

// TestFormatModelInfo: the info table carries values and sources but
// never the endpoint address or key material.
func TestFormatModelInfo(t *testing.T) {
	r := Resolved{Sources: map[string]Source{}}
	r = ResolveLimits(modelWith("https://secret-endpoint.invalid/v1", "gpt-4o"), ResolveOptions{})
	out := FormatModelInfo("gpt-4o", r)
	if !strings.Contains(out, "context_tokens:") || !strings.Contains(out, "builtin") {
		t.Fatalf("info output missing fields/sources:\n%s", out)
	}
	if strings.Contains(out, "secret-endpoint") || strings.Contains(out, "http") {
		t.Fatalf("info output leaks endpoint material:\n%s", out)
	}
	if !strings.Contains(out, "audio:") || !strings.Contains(out, "video:") {
		t.Fatalf("info output missing capability flags:\n%s", out)
	}
}

// TestParseProbeEntryVariants: both vendor field spellings parse.
func TestParseProbeEntryVariants(t *testing.T) {
	e := map[string]json.RawMessage{}
	for k, v := range map[string]string{
		"context_length": "32000", "max_completion_tokens": "4096",
		"supports_images": "true", "supports_tools": "true",
		"supports_audio": "false", "supports_video": "false",
	} {
		e[k] = json.RawMessage(v)
	}
	p := parseProbeEntry(e)
	if p.contextTokens == nil || *p.contextTokens != 32000 ||
		p.maxOutput == nil || *p.maxOutput != 4096 {
		t.Fatalf("variant parse failed: %+v", p)
	}
}
