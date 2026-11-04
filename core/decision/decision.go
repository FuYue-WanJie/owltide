// Package decision appends one JSON line per agent turn to
// ~/.owltide/logs/decisions.jsonl. The field set is v1-frozen:
// future routing / reply-policy models will train on this file,
// so fields must stay stable — only additive changes allowed.
package decision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ToolRecord captures one tool invocation inside a turn.
type ToolRecord struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// DurationMS is the tool's own wall time.
	DurationMS int64 `json:"duration_ms"`
	// Error is empty on success.
	Error string `json:"error,omitempty"`
}

// Record is one turn's decision line.
type Record struct {
	// TS is the turn start time (RFC3339).
	TS string `json:"ts"`
	// SessionID groups turns of one conversation.
	SessionID string `json:"session_id"`
	// Turn is the 1-based index within the session.
	Turn int `json:"turn"`
	// InputDigest summarizes the user input (first 200 runes).
	InputDigest string `json:"input_digest"`
	// Model is the model name used this turn.
	Model string `json:"model"`
	// PromptTokens / CompletionTokens from provider usage.
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// ToolsCalled lists tool invocations in order.
	ToolsCalled []ToolRecord `json:"tools_called"`
	// RepliedDirectly is true when the turn answered without any tool.
	RepliedDirectly bool `json:"replied_directly"`
	// DurationMS is the whole turn's wall time.
	DurationMS int64 `json:"duration_ms"`
	// Outcome: "success" | "error" | "user_correction".
	Outcome string `json:"outcome"`
	// Error carries the failure reason when Outcome != success.
	Error string `json:"error,omitempty"`
}

// Outcome values.
const (
	OutcomeSuccess        = "success"
	OutcomeError          = "error"
	OutcomeUserCorrection = "user_correction"
)

// Logger appends records safely from concurrent turns.
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// Open creates (append mode) the log file under dir.
func Open(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "decisions.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

// Log writes one record as a single JSON line.
func (l *Logger) Log(r Record) error {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(line, '\n'))
	return err
}

// Close flushes and closes the file.
func (l *Logger) Close() error { return l.f.Close() }
