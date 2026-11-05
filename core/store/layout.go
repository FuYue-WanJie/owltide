// Package store: directory layout and commit metadata helpers.
package store

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Layout of the ~/.owltide configuration directory.
var Dirs = []string{"agents", "skills", "memory", "channels", "logs"}

const gitignore = "logs/\n"

// EnsureLayout creates the directory skeleton and .gitignore.
func EnsureLayout(dir string) error {
	for _, d := range Dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); os.IsNotExist(err) {
		return os.WriteFile(gi, []byte(gitignore), 0o644)
	}
	return nil
}

// CommitMeta describes who and why for one automatic commit.
type CommitMeta struct {
	Who    string // "agent" or "user"
	Tool   string // self-evolution tool name, "" for user edits
	Game   string // session id, optional
	Reason string // human-readable summary
}

// Message renders the commit message; also parsed back by ParseMessage.
func (m CommitMeta) Message() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(m.Reason) + "\n")
	if m.Tool != "" {
		b.WriteString("\nwho: " + m.Who + "\ntool: " + m.Tool + "\n")
	} else {
		b.WriteString("\nwho: " + m.Who + "\n")
	}
	if m.Game != "" {
		b.WriteString("session: " + m.Game + "\n")
	}
	return b.String()
}

// ParseMessage extracts meta back out of a commit message.
func ParseMessage(msg string) (reason, who, tool, session string) {
	head, tail := msg, ""
	if i := strings.Index(msg, "\nwho: "); i >= 0 {
		head, tail = msg[:i], msg[i+1:]
	}
	reason = strings.TrimSpace(head)
	for _, line := range strings.Split(tail, "\n") {
		switch {
		case strings.HasPrefix(line, "who: "):
			who = strings.TrimPrefix(line, "who: ")
		case strings.HasPrefix(line, "tool: "):
			tool = strings.TrimPrefix(line, "tool: ")
		case strings.HasPrefix(line, "session: "):
			session = strings.TrimPrefix(line, "session: ")
		}
	}
	return
}

// AutoCommit snapshots the tree with meta attached. A no-op returns
// ("", false, nil) when nothing changed.
func (s *Store) AutoCommit(meta CommitMeta) (string, bool, error) {
	if s.Who == "" || meta.Who == "" {
		s.Who = "owltide"
	}
	id, changed, err := s.snapshot(meta.Message())
	if err != nil || id == "" || changed == nil {
		return id, len(changed) > 0, err
	}
	return id, true, nil
}

// toRFC3339 converts unix seconds to RFC3339 UTC.
func toRFC3339(unixSec string) string {
	var sec int64
	for _, c := range unixSec {
		sec = sec*10 + int64(c-'0')
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// EnsureInit makes the first commit when HEAD is unborn, so later
// rollbacks always have a parent to return to.
func (s *Store) EnsureInit() error {
	head, err := s.Head()
	if err != nil {
		return err
	}
	if head != "" {
		return nil
	}
	_, _, err = s.AutoCommit(CommitMeta{Who: "user", Reason: "init"})
	return err
}
