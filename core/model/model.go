// Package model implements an OpenAI-compatible chat client with
// streaming and tool calls. Deliberately dependency-free: SSE is
// parsed with bufio over the standard net/http response.
package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is the model's request to invoke a tool.
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function names the tool and carries raw JSON arguments.
type Function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage is token accounting from the provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Total returns the combined token count.
func (u Usage) Total() int { return u.PromptTokens + u.CompletionTokens }

// Request is the chat completion payload.
type Request struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Tools      []any     `json:"tools,omitempty"`
	Stream     bool      `json:"stream"`
	StreamOpts struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// client talks to one OpenAI-compatible endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewClient builds a client; httpClient may be nil for the default.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{BaseURL: baseURL, APIKey: apiKey, HTTP: httpClient}
}

// chatRequest builds and sends the HTTP request for a streaming call.
func (c *Client) chatRequest(ctx context.Context, req Request) (*http.Response, error) {
	req.Stream = true
	req.StreamOpts.IncludeUsage = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return c.HTTP.Do(httpReq)
}

// StreamResult carries one streaming callback chunk.
type StreamResult struct {
	// Content is an incremental text delta (may be empty).
	Content string
	// ToolCalls are incremental tool call deltas.
	ToolCalls []ToolCall
}

// Stream performs a streaming chat completion. onDelta is called for
// each chunk; the final aggregated message and usage are returned.
// Tool call argument fragments are stitched together by index.
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(StreamResult)) (Message, Usage, error) {
	resp, err := c.chatRequest(ctx, req)
	if err != nil {
		return Message{}, Usage{}, fmt.Errorf("model request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return Message{}, Usage{}, fmt.Errorf("model http %d: %s", resp.StatusCode, clampErr(b.String()))
	}

	var (
		aggContent strings.Builder
		aggCalls   = map[int]*ToolCall{}
		usage      Usage
		sc         = bufio.NewScanner(resp.Body)
	)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return Message{}, usage, fmt.Errorf("bad stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta.Content != "" {
				aggContent.WriteString(delta.Content)
				if onDelta != nil {
					onDelta(StreamResult{Content: delta.Content})
				}
			}
			for i, tc := range delta.ToolCalls {
				idx := tc.Index
				if idx == 0 && len(delta.ToolCalls) == 1 {
					idx = i // tolerate providers that omit index
				}
				agg := aggCalls[idx]
				if agg == nil {
					agg = &ToolCall{Type: "function"}
					aggCalls[idx] = agg
				}
				if tc.ID != "" {
					agg.ID = tc.ID
				}
				if tc.Type != "" {
					agg.Type = tc.Type
				}
				if tc.Function.Name != "" {
					agg.Function.Name += tc.Function.Name
				}
				agg.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, usage, fmt.Errorf("stream read: %w", err)
	}

	msg := Message{Role: RoleAssistant, Content: aggContent.String()}
	// Emit calls ordered by index.
	for i := 0; i < len(aggCalls); i++ {
		if tc, ok := aggCalls[i]; ok {
			msg.ToolCalls = append(msg.ToolCalls, *tc)
		}
	}
	return msg, usage, nil
}

func clampErr(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
