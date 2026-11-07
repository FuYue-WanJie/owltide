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

// RepairTornTail reads an existing session log and, when its tail is a
// torn line (crash residue), truncates the file back to the end of the
// last clean record. Without this, the next append would glue onto the
// broken line and a later rebuild would discard a good record with it.
// Returns the parsed messages and the warning (empty when the file
// ended on a clean record boundary).
func RepairTornTail(path string) (msgs []model.Message, warning string, err error) {
	msgs, warning, err = RebuildWithWarning(path)
	if err != nil || warning == "" {
		return msgs, warning, err
	}
	clean, terr := lastCleanBoundary(path)
	if terr != nil {
		return nil, "", terr
	}
	if err := os.Truncate(path, clean); err != nil {
		return nil, "", fmt.Errorf("sessionlog: truncate torn tail: %w", err)
	}
	return msgs, warning, nil
}

// lastCleanBoundary returns the file size up to the end of the last
// line that parses as a complete record.
func lastCleanBoundary(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var boundary int64
	for i, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			boundary += int64(len(line))
			continue
		}
		var m model.Message
		if json.Unmarshal(trimmed, &m) != nil {
			// First bad line: everything from here on is dropped.
			_ = i
			break
		}
		boundary += int64(len(line))
	}
	return boundary, nil
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
