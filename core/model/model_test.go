package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStreamServer mimics an OpenAI-compatible SSE endpoint.
// scenario returns the response body for a given request.
func fakeStreamServer(t *testing.T, scenario func(req Request) string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		var req Request
		if err := decodeBody(r, &req); err != nil {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(scenario(req)))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-key", srv.Client()), srv
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func sse(chunks ...string) string {
	var out strings.Builder
	for _, c := range chunks {
		out.WriteString("data: " + c + "\n\n")
	}
	out.WriteString("data: [DONE]\n\n")
	return out.String()
}

func TestStreamTextAndUsage(t *testing.T) {
	client, _ := fakeStreamServer(t, func(req Request) string {
		return sse(
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		)
	})
	var got strings.Builder
	msg, usage, err := client.Stream(context.Background(), Request{Model: "m"}, func(sr StreamResult) {
		got.WriteString(sr.Content)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "Hello" || msg.Content != "Hello" {
		t.Fatalf("content: %q", got.String())
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestStreamToolCallsStitch(t *testing.T) {
	client, _ := fakeStreamServer(t, func(req Request) string {
		return sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"run_","arguments":"{\"co"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"shell","arguments":"mmand\":\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x\"}"}}]}}]}`,
		)
	})
	msg, _, err := client.Stream(context.Background(), Request{Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("want 2 calls, got %+v", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "c1" || tc.Function.Name != "run_shell" || tc.Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("stitched call: %+v", tc)
	}
	if msg.ToolCalls[1].Function.Name != "read_file" {
		t.Fatalf("second call: %+v", msg.ToolCalls[1])
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "k", srv.Client())
	_, _, err := client.Stream(context.Background(), Request{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want http error, got %v", err)
	}
}
