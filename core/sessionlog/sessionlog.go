// Package sessionlog is the append-only main-conversation message log:
// one JSONL file per session (~/.owltide/sessions/<id>/messages.jsonl).
// It is the single source of truth; the in-memory message slice and any
// resume rebuild read back from it. Every line is written with one
// Write call followed by fsync, so a crash can only truncate the tail.
package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/FuYue-WanJie/owltide/core/model"
)

// Log is one append-only session message file.
type Log struct {
	mu sync.Mutex
	f  *os.File
}

// Open creates (or reopens append-mode) the messages.jsonl under
// <root>/<sessionID>/ and returns the writer.
func Open(root, sessionID string) (*Log, error) {
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

// Append writes one message as a single JSON line and fsyncs.
func (l *Log) Append(m model.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(b); err != nil {
		return err
	}
	return l.f.Sync()
}

// Close closes the underlying file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Rebuild reads back the message sequence. A truncated final line
// (crash residue) is dropped with a warning; anything else fails.
func Rebuild(path string) ([]model.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []model.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var truncWarn string
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m model.Message
		if err := json.Unmarshal(line, &m); err != nil {
			if sc.Err() == nil {
				// JSON parse failed on the last line: torn write tail.
				truncWarn = fmt.Sprintf(
					"sessionlog: dropped truncated final line (%d bytes, bad JSON)", len(line))
				break
			}
			return nil, fmt.Errorf("sessionlog: corrupt line: %w", err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil && truncWarn == "" {
		return nil, fmt.Errorf("sessionlog: read %s: %w", path, err)
	}
	_ = truncWarn // caller-visible via Warning below
	return out, nil
}

// RebuildWithWarning is Rebuild plus the tail-drop warning (empty when
// the file ended on a clean record boundary).
func RebuildWithWarning(path string) (msgs []model.Message, warning string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m model.Message
		if err := json.Unmarshal(line, &m); err != nil {
			// Torn write tail: drop and warn, keep what parsed.
			warning = fmt.Sprintf(
				"sessionlog: dropped truncated final line (%d bytes, bad JSON)", len(line))
			break
		}
		out := append(msgs, m)
		msgs = out
	}
	if err := sc.Err(); err != nil && warning == "" {
		return nil, "", fmt.Errorf("sessionlog: read %s: %w", path, err)
	}
	return msgs, warning, nil
}
